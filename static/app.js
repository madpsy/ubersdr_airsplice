/* AirSplice — app.js */
'use strict';

const BASE = window.BASE_PATH || '';
const PAGE_SIZE = 50;

// ── Bandwidth helpers ─────────────────────────────────────────────────────────

// Per-mode bandwidth config: { default (Hz), min (Hz), max (Hz) }
const BW_CONFIG = {
  usb:  { default: 2700, min:   300, max: 6000, step: 50 },
  lsb:  { default: 2700, min:   300, max: 6000, step: 50 },
  am:   { default: 5000, min:   500, max: 6000, step: 50 },
  sam:  { default: 5000, min:   500, max: 6000, step: 50 },
  fm:   { default: 8000, min:  1000, max: 8000, step: 100 },
  nfm:  { default: 8000, min:  1000, max: 8000, step: 100 },
  cw:   { default:  500, min:    50, max:  500, step: 10 },
};

function bwCfg(mode) {
  return BW_CONFIG[mode] || BW_CONFIG['usb'];
}

function fmtBW(hz) {
  if (hz >= 1000) return (hz / 1000).toFixed(hz % 1000 === 0 ? 1 : 2) + ' kHz';
  return hz + ' Hz';
}

// Configure a bandwidth slider + label pair for the given mode.
// slider: <input type="range">, label: <span> showing current value.
// currentHz: initial value in Hz (0 = use mode default).
function configureBWSlider(slider, label, mode, currentHz) {
  const cfg = bwCfg(mode);
  const val = (currentHz > 0) ? currentHz : cfg.default;
  slider.min   = cfg.min;
  slider.max   = cfg.max;
  slider.step  = cfg.step;
  slider.value = Math.max(cfg.min, Math.min(cfg.max, val));
  label.textContent = fmtBW(parseInt(slider.value, 10));
  slider.oninput = () => { label.textContent = fmtBW(parseInt(slider.value, 10)); };
}

// ── VOX alert bell ───────────────────────────────────────────────────────────

// localStorage key: 'vox_bell_{label}' → '1' (enabled) or '0' (disabled)
function bellKey(label) { return 'vox_bell_' + label; }
function isBellEnabled(label) { return localStorage.getItem(bellKey(label)) === '1'; }
function setBell(label, on) { localStorage.setItem(bellKey(label), on ? '1' : '0'); }

// Synthesise a short two-tone ding using Web Audio API (no external file needed).
let _dingCtx = null;
function playDing() {
  try {
    if (!_dingCtx) _dingCtx = new (window.AudioContext || window.webkitAudioContext)();
    const ctx = _dingCtx;
    const now = ctx.currentTime;
    [[880, 0, 0.12], [1320, 0.1, 0.18]].forEach(([freq, start, end]) => {
      const osc  = ctx.createOscillator();
      const gain = ctx.createGain();
      osc.type = 'sine';
      osc.frequency.value = freq;
      gain.gain.setValueAtTime(0.35, now + start);
      gain.gain.exponentialRampToValueAtTime(0.001, now + end);
      osc.connect(gain);
      gain.connect(ctx.destination);
      osc.start(now + start);
      osc.stop(now + end + 0.01);
    });
  } catch (_) {}
}

// Today's date in YYYY-MM-DD (local wall-clock, matching what the server uses for UTC).
// We use UTC here to stay consistent with the server's date bucketing.
function todayUTC() {
  const d = new Date();
  const pad = n => String(n).padStart(2, '0');
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth()+1)}-${pad(d.getUTCDate())}`;
}

let state = {
  offset: 0,
  channelId: '', // UUID of the selected channel filter ('' = all channels)
  date: todayUTC(), // default: today
  total: 0,
  authed: false,
  passwordConfigured: false,
  pendingDeleteId: null,
};

// ── Utilities ────────────────────────────────────────────────────────────────

function api(path, opts) {
  return fetch(BASE + path, opts).then(r => {
    if (!r.ok) return r.text().then(t => Promise.reject(t));
    return r.json();
  });
}

function fmtFreq(hz) {
  if (hz >= 1e6) return (hz / 1e6).toFixed(3) + ' MHz';
  if (hz >= 1e3) return (hz / 1e3).toFixed(1) + ' kHz';
  return hz + ' Hz';
}

function fmtDuration(secs) {
  if (!secs) return '—';
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = Math.floor(secs % 60);
  if (h > 0) return `${h}h ${m}m ${s}s`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

function fmtDate(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return d.toLocaleString();
}

function fmtSNR(snr) {
  if (!snr || !snr.count || snr.avg_db == null) return '—';
  return `${snr.avg_db.toFixed(1)} dB`;
}

// ── Auth ─────────────────────────────────────────────────────────────────────

function renderAuthBar() {
  const bar = document.getElementById('auth-bar');
  if (!state.passwordConfigured) { bar.innerHTML = ''; return; }
  if (state.authed) {
    bar.innerHTML = `<span style="color:var(--muted);font-size:0.8rem">Authenticated</span>
      <button id="logout-btn">Logout</button>`;
    document.getElementById('logout-btn').onclick = doLogout;
  } else {
    bar.innerHTML = `<button id="login-btn">Login</button>`;
    document.getElementById('login-btn').onclick = () => showModal();
  }
  // Show/hide the "Add Channel" button based on auth state
  const addBtn = document.getElementById('add-channel-btn');
  if (addBtn) {
    if (state.authed) {
      addBtn.classList.remove('hidden');
    } else {
      addBtn.classList.add('hidden');
      hideAddChannelForm();
    }
  }
}

function showModal() {
  document.getElementById('login-modal').classList.remove('hidden');
  document.getElementById('login-password').value = '';
  document.getElementById('login-error').classList.add('hidden');
  document.getElementById('login-password').focus();
}

function hideModal() {
  document.getElementById('login-modal').classList.add('hidden');
}

document.getElementById('login-cancel').onclick = hideModal;
document.getElementById('login-submit').onclick = doLogin;
document.getElementById('login-password').addEventListener('keydown', e => {
  if (e.key === 'Enter') doLogin();
});

function doLogin() {
  const pw = document.getElementById('login-password').value;
  api('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: pw }),
  }).then(() => {
    state.authed = true;
    hideModal();
    renderAuthBar();
    loadChannels();
    loadRecordings();
  }).catch(() => {
    const err = document.getElementById('login-error');
    err.textContent = 'Incorrect password.';
    err.classList.remove('hidden');
  });
}

function doLogout() {
  api('/api/auth/logout', { method: 'POST' }).finally(() => {
    state.authed = false;
    renderAuthBar();
    loadChannels();
    loadRecordings();
  });
}

function checkAuth() {
  return api('/api/auth/status').then(d => {
    state.passwordConfigured = d.password_configured;
    state.authed = d.authenticated;
    renderAuthBar();
  });
}

// ── Retention ─────────────────────────────────────────────────────────────────

// Cache of {default_hours, channels:{label:hours}} loaded from /api/retention
let retentionState = { default_hours: 0, channels: {} };

function loadRetention() {
  api('/api/retention').then(d => {
    retentionState = d || { default_hours: 0, channels: {} };
    // Update any already-rendered channel cards
    document.querySelectorAll('.channel-card[data-label]').forEach(card => {
      const label = card.dataset.label;
      const sel = card.querySelector('.ch-retention-select');
      if (sel) {
        const hours = (retentionState.channels && retentionState.channels[label]) || retentionState.default_hours || 0;
        sel.value = String(hours);
      }
    });
  }).catch(() => {});
}

// ── Date filter ───────────────────────────────────────────────────────────────

// Fetch available recording dates from /api/dates and populate the #date-filter
// <select>.  Dates that have recordings get a bullet prefix so the user can see
// at a glance which days have data.  The currently-selected date is preserved.
// loadDates([thenFn]) — refresh the date dropdown, optionally calling thenFn
// after the date is resolved (so callers that need the correct state.date can
// wait for it rather than racing with the async fetch).
function loadDates(thenFn) {
  // Capture the current selection synchronously from the DOM *before* the async
  // fetch so that any user interaction that happened between the call and the
  // promise resolution is not lost.  We also keep state.date in sync below.
  const sel = document.getElementById('date-filter');
  const snapshotDate = sel ? sel.value : state.date;
  // Pass the current channel UUID filter so only dates with recordings for
  // that channel are shown.  Empty string = all channels.
  const channelParam = state.channelId ? `?channel_id=${encodeURIComponent(state.channelId)}` : '';

  api('/api/dates' + channelParam).then(d => {
    if (!sel) return;
    const available = new Set(d.dates || []);

    // Rebuild options: one option per available date, plus today even if empty.
    const today = todayUTC();
    const datesSet = new Set([today, ...available]);
    // Sort newest-first.
    const sorted = Array.from(datesSet).sort((a, b) => b.localeCompare(a));

    // Use the DOM snapshot as the value to restore (more reliable than state.date
    // because it reflects any selection the user made before the fetch resolved).
    const current = snapshotDate || today;

    // Clear all existing options and rebuild.
    while (sel.options.length > 0) sel.remove(0);

    sorted.forEach(dateStr => {
      const opt = document.createElement('option');
      opt.value = dateStr;
      // Friendly label: "Today", "Yesterday", or the date string.
      const diffDays = Math.round((new Date(today) - new Date(dateStr)) / 86400000);
      let label = dateStr;
      if (diffDays === 0) label = `Today (${dateStr})`;
      else if (diffDays === 1) label = `Yesterday (${dateStr})`;
      // Bullet prefix when there are recordings on that day.
      opt.textContent = (available.has(dateStr) ? '● ' : '○ ') + label;
      sel.appendChild(opt);
    });

    // Restore selection (fall back to today if the previous value is gone).
    if (Array.from(sel.options).some(o => o.value === current)) {
      sel.value = current;
      state.date = current;
    } else {
      sel.value = today;
      state.date = today;
    }

    if (thenFn) thenFn();
  }).catch(() => { if (thenFn) thenFn(); });
}

// ── Quota ─────────────────────────────────────────────────────────────────────

// Cache of {overall_mb, channels:{label:mb}} loaded from /api/quota
let quotaState = { overall_mb: 0, channels: {} };

function loadQuota() {
  api('/api/quota').then(d => {
    quotaState = d || { overall_mb: 0, channels: {} };
    // Update any already-rendered channel cards
    document.querySelectorAll('.channel-card[data-label]').forEach(card => {
      const label = card.dataset.label;
      const inp = card.querySelector('.ch-quota-input');
      if (inp) {
        const mb = (quotaState.channels && quotaState.channels[label]) || 0;
        inp.value = String(mb);
      }
    });
  }).catch(() => {});
}

function setChannelRetention(label, hours) {
  if (!state.authed) {
    alert('You must be logged in to change the retention period.');
    loadRetention();
    return;
  }
  api('/api/retention', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ label, keep_hours: hours }),
  }).then(d => {
    retentionState = d || retentionState;
  }).catch(err => {
    alert('Failed to update retention: ' + err);
    loadRetention();
  });
}

function retentionOptionsHTML() {
  const opts = [
    [0,   'Keep forever'],
    [1,   'Keep 1 hour'],
    [2,   'Keep 2 hours'],
    [6,   'Keep 6 hours'],
    [12,  'Keep 12 hours'],
    [24,  'Keep 24 hours'],
    [48,  'Keep 2 days'],
    [168, 'Keep 7 days'],
    [336, 'Keep 14 days'],
    [720, 'Keep 30 days'],
  ];
  return opts.map(([v, t]) => `<option value="${v}">${t}</option>`).join('');
}

// ── Channels ─────────────────────────────────────────────────────────────────

function loadChannels() {
  api('/api/channels').then(channels => {
    renderChannels(channels || []);
  }).catch(e => console.warn('channels:', e));
}

// Returns true if a channel card's settings panel is open or the user is
// actively interacting with it (rename form visible, live player active).
function channelCardIsActive(card) {
  if (!card) return false;
  if (card._livePlayer) return true;
  const panel = card.querySelector('.sr-settings-panel');
  if (panel && !panel.classList.contains('hidden')) return true;
  const renameForm = card.querySelector('.ch-rename-form');
  if (renameForm && !renameForm.classList.contains('hidden')) return true;
  return false;
}

// Add channels to the #label-filter <select> without removing any existing ones.
// Each channel is an object {id: UUID, label: string}.
// The option value is the channel UUID so API calls use stable identifiers.
// Removal is handled by syncLabelFilter() which is called after recordings load
// so it can keep channels that have recordings even if the channel is gone.
function addChannelLabelsToFilter(channels) {
  const filter = document.getElementById('label-filter');
  const existing = new Set(Array.from(filter.options).map(o => o.value));
  channels.forEach(ch => {
    // ch may be a plain string (legacy) or {id, label}.
    const id    = (typeof ch === 'object') ? (ch.id    || ch.label) : ch;
    const label = (typeof ch === 'object') ? (ch.label || ch.id)   : ch;
    if (id && !existing.has(id)) {
      const opt = document.createElement('option');
      opt.value = id;
      opt.textContent = label;
      filter.appendChild(opt);
    }
  });
}

// Rebuild the #label-filter options to contain exactly the union of active
// channels and channels present in the current sessions list.
// activeChannels: array of {id, label} from renderChannels
// sessionChannels: array of {id, label} from the sessions API response
// Preserves the current selection (resets to "All" if the selected channel UUID
// is no longer in either set).
function syncLabelFilter(activeChannels, sessionChannels) {
  const filter = document.getElementById('label-filter');
  // Build a map of id→label for all valid channels.
  const validIds = new Map();
  [...activeChannels, ...sessionChannels].forEach(ch => {
    const id    = (typeof ch === 'object') ? (ch.id    || ch.label) : ch;
    const label = (typeof ch === 'object') ? (ch.label || ch.id)   : ch;
    if (id) validIds.set(id, label);
  });
  // Remove options that are no longer valid (keep the "All" sentinel).
  Array.from(filter.options).forEach(opt => {
    if (opt.value && !validIds.has(opt.value)) opt.remove();
  });
  // Add any new channels.
  addChannelLabelsToFilter(Array.from(validIds.entries()).map(([id, label]) => ({ id, label })));
  // If the currently-selected channel was removed, reset to "All".
  if (state.channelId && !validIds.has(state.channelId)) {
    state.channelId = '';
    filter.value = '';
  }
}

function renderChannels(channels) {
  const grid = document.getElementById('channels-grid');

  // Add any new channels to the filter (don't remove — syncLabelFilter
  // handles removal once we also know which channels have recordings).
  addChannelLabelsToFilter(channels.map(ch => ({ id: ch.channel_id || ch.label, label: ch.label })));

  const activeLabels = new Set(channels.map(ch => ch.label));

  // Remove cards for channels that no longer exist.
  Array.from(grid.querySelectorAll('.channel-card[data-label]')).forEach(card => {
    if (!activeLabels.has(card.dataset.label)) card.remove();
  });

  if (!channels.length) {
    grid.innerHTML = '<div class="empty">No channels configured. Add one above.</div>';
    return;
  }

  channels.forEach((ch, idx) => {
    const existingCard = grid.querySelector(`.channel-card[data-label="${CSS.escape(ch.label)}"]`);

    // If the settings panel is open or user is interacting, do a soft update only.
    if (existingCard && channelCardIsActive(existingCard)) {
      // Update only the dynamic parts that don't affect the settings panel.
      const sr = ch.smart_record || {};
      const srEnabled = !!sr.enabled;
      const gateState = ch.smart_record_gate || '';
      const sched = ch.schedule || {};
      const schedEnabled = !!sched.enabled;
      const schedActive = !!ch.schedule_active;

      let statusClass, statusLabel;
      if (srEnabled) {
        const gate = gateState || 'idle';
        const gateLabels = {
          idle:      ['stopped',   '◌ Idle'],
          arming:    ['arming',    '◔ Arming'],
          recording: ['recording', '● Recording'],
          tail:      ['tail',      '◕ Tail'],
        };
        [statusClass, statusLabel] = gateLabels[gate] || ['stopped', gate];
      } else if (schedEnabled && !schedActive) {
        statusClass = 'sched-waiting';
        statusLabel = '⏰ Waiting';
      } else {
        statusClass = ch.recording ? 'recording' : (ch.status || 'stopped');
        statusLabel = ch.recording ? '● Recording' : (ch.status || 'stopped');
      }

      const statusEl = existingCard.querySelector('.ch-status');
      if (statusEl) {
        statusEl.className = `ch-status ${statusClass}`;
        statusEl.textContent = statusLabel;
      }
      const snrEl = existingCard.querySelector('.ch-snr');
      if (snrEl) snrEl.textContent = 'SNR: ' + fmtSNR(ch.snr);

      // Ensure correct DOM position.
      const cards = Array.from(grid.querySelectorAll('.channel-card'));
      if (cards.indexOf(existingCard) !== idx) {
        const ref = grid.querySelectorAll('.channel-card')[idx];
        if (ref && ref !== existingCard) grid.insertBefore(existingCard, ref);
      }
      return; // skip full rebuild
    }

    const card = document.createElement('div');
    card.className = 'channel-card';
    card.dataset.label = ch.label;
    card.dataset.channelId = ch.channel_id || '';

    // Smart record state — needed early for status derivation.
    const sr = ch.smart_record || {};
    const srEnabled = !!sr.enabled;
    const gateState = ch.smart_record_gate || '';

    // Schedule state
    const sched = ch.schedule || {};
    const schedEnabled = !!sched.enabled;
    const schedActive = !!ch.schedule_active;

    // For VOX channels derive the displayed status from the gate state so the
    // card shows idle / arming / recording / tail instead of the generic
    // connection status ("running").
    let statusClass, statusLabel;
    if (srEnabled) {
      const gate = gateState || 'idle';
      const gateLabels = {
        idle:      ['stopped',   '◌ Idle'],
        arming:    ['arming',    '◔ Arming'],
        recording: ['recording', '● Recording'],
        tail:      ['tail',      '◕ Tail'],
      };
      [statusClass, statusLabel] = gateLabels[gate] || ['stopped', gate];
    } else if (schedEnabled && !schedActive) {
      statusClass = 'sched-waiting';
      statusLabel = '⏰ Waiting';
    } else {
      statusClass = ch.recording ? 'recording' : (ch.status || 'stopped');
      statusLabel = ch.recording ? '● Recording' : (ch.status || 'stopped');
    }

    const removeBtn = state.authed
      ? `<button class="remove-ch-btn" onclick="removeChannel('${ch.label}')">✕ Remove</button>`
      : '';
    const renameBtn = state.authed
      ? `<button class="rename-ch-btn" onclick="showRenameChannel(this,'${ch.label}')">✎ Rename</button>`
      : '';
    const listenBtn = (ch.recording || ch.status === 'running')
      ? `<button class="ch-live-btn" onclick="toggleLivePlayer(this,'${ch.label}')">▶ Listen live</button>`
      : '';

    // Retention dropdown — only shown when authenticated
    const retentionHours = (retentionState.channels && retentionState.channels[ch.label]) || retentionState.default_hours || 0;
    const retentionSel = state.authed
      ? `<label class="ch-retention-label">Keep:
           <select class="ch-retention-select" data-label="${ch.label}">
             ${retentionOptionsHTML()}
           </select>
         </label>`
      : '';

    // Smart record badge + settings panel (srEnabled/gateState already declared above)
    const srBadge = srEnabled
      ? `<span class="sr-badge sr-badge-${gateState || 'idle'}" title="Signal Activated recording — gate: ${gateState || 'idle'}">⚡ VOX</span>`
      : '';

    // Schedule badge
    const schedBadge = schedEnabled
      ? `<span class="sched-badge ${schedActive ? 'sched-badge-active' : 'sched-badge-waiting'}" title="Scheduled recording — ${schedActive ? 'window open' : 'waiting for window'}">⏰ Sched</span>`
      : '';

    // Next transition hint (shown on badge tooltip via title, also as text below)
    let schedNextHint = '';
    if (schedEnabled && ch.schedule_next_transition) {
      const nextDate = new Date(ch.schedule_next_transition);
      const action = ch.schedule_next_active ? 'starts' : 'stops';
      schedNextHint = `<div class="ch-sched-next">Next ${action}: ${nextDate.toLocaleString()}</div>`;
    }

    // Smart record + schedule settings panel (auth only)
    const srPanel = state.authed ? buildSmartRecordPanel(ch) : '';

    // Effective bandwidth: use stored value or fall back to mode default.
    const mode = ch.audio_mode || 'usb';
    const effectiveBwHz = (ch.bandwidth_hz && ch.bandwidth_hz > 0)
      ? ch.bandwidth_hz
      : bwCfg(mode).default;
    const bwDisplay = fmtBW(effectiveBwHz);

    card.innerHTML = `
      <div class="ch-label-row">
        <div class="ch-label">${ch.label}</div>
        ${srBadge}
        ${schedBadge}
        ${renameBtn}
      </div>
      <div class="ch-rename-form hidden">
        <input class="ch-rename-input" type="text" value="${ch.label}" placeholder="New name">
        <button class="ch-rename-save" onclick="doRenameChannel(this)" title="Save">✓</button>
        <button class="ch-rename-cancel" onclick="hideRenameChannel(this)" title="Cancel">✕</button>
      </div>
      <div class="ch-freq-row">
        <div class="ch-freq">${fmtFreq(ch.freq_hz)}</div>
        <div class="ch-mode">${(ch.audio_mode || '').toUpperCase()}</div>
        <div class="ch-bw">${bwDisplay}</div>
      </div>
      <span class="ch-status ${statusClass}">${statusLabel}</span>
      ${schedNextHint}
      <div class="ch-snr">SNR: ${fmtSNR(ch.snr)}</div>
      <canvas class="ch-snr-chart" height="32" title="Rolling 10s SNR (30 dB=red → 60 dB=green)"></canvas>
      <div class="ch-actions">
        ${listenBtn}
        ${removeBtn}
      </div>
      ${retentionSel}
      ${srPanel}`;

    // Set the retention dropdown value after inserting into DOM
    if (state.authed) {
      const sel = card.querySelector('.ch-retention-select');
      if (sel) {
        sel.value = String(retentionHours);
        sel.onchange = e => setChannelRetention(ch.label, parseInt(e.target.value, 10));
      }
    }

    // Wire bell button toggle (available to all users, not just authed)
    const bellBtnEl = card.querySelector('.ch-bell-btn');
    if (bellBtnEl) {
      bellBtnEl.onclick = () => {
        const nowOn = !isBellEnabled(ch.label);
        setBell(ch.label, nowOn);
        bellBtnEl.textContent = nowOn ? '🔔' : '🔕';
        bellBtnEl.classList.toggle('ch-bell-on', nowOn);
        bellBtnEl.title = nowOn ? 'Alert on: click to disable' : 'Alert off: click to enable';
      };
    }

    // Wire up settings panel toggle, bandwidth slider, save button, and rec-mode select
    if (state.authed) {
      const srToggleBtn = card.querySelector('.sr-settings-toggle');
      const srSettingsDiv = card.querySelector('.sr-settings-panel');
      if (srToggleBtn && srSettingsDiv) {
        srToggleBtn.onclick = () => srSettingsDiv.classList.toggle('hidden');
      }
      // Wire bandwidth slider live label update
      const srBwSlider = card.querySelector('.sr-bw-slider');
      const srBwLabel  = card.querySelector('.sr-bw-value');
      if (srBwSlider && srBwLabel) {
        srBwSlider.oninput = () => { srBwLabel.textContent = fmtBW(parseInt(srBwSlider.value, 10)); };
      }
      const srSaveBtn = card.querySelector('.sr-save-btn');
      if (srSaveBtn) {
        srSaveBtn.onclick = () => saveSmartRecord(card, ch.label);
      }
      // Wire the rec-mode select inside the card to show/hide threshold/schedule fields
      const srModeSelect = card.querySelector('.sr-mode-select');
      const srThreshFields = card.querySelector('.sr-thresh-fields');
      const schedFields = card.querySelector('.sched-fields');
      if (srModeSelect) {
        srModeSelect.onchange = () => {
          if (srThreshFields) srThreshFields.classList.toggle('hidden', srModeSelect.value !== 'smart');
          if (schedFields)    schedFields.classList.toggle('hidden', srModeSelect.value !== 'scheduled');
        };
      }
      // Wire schedule editor dynamic behaviour (add/remove rules)
      wireScheduleEditor(card);
    }

    // Insert at correct position (or append if no reference card).
    const refCard = grid.querySelectorAll('.channel-card')[idx] || null;
    if (refCard && refCard !== existingCard) {
      grid.insertBefore(card, refCard);
      if (existingCard) existingCard.remove();
    } else if (!existingCard) {
      grid.appendChild(card);
    } else {
      grid.insertBefore(card, existingCard);
      existingCard.remove();
    }
  });
}

// Build the HTML for the smart-record settings panel inside a channel card.
function buildSmartRecordPanel(ch) {
  const sr = ch.smart_record || {};
  const enabled = !!sr.enabled;
  const startThresh   = sr.start_thresh_db  != null ? sr.start_thresh_db  : 50;
  const startHold     = sr.start_hold_sec   != null ? sr.start_hold_sec   : 2;
  const stopThresh    = sr.stop_thresh_db   != null ? sr.stop_thresh_db   : 35;
  const stopHold      = sr.stop_hold_sec    != null ? sr.stop_hold_sec    : 5;
  const maxRecordMins = sr.max_record_mins  != null ? sr.max_record_mins  : 0;

  const threshHidden = enabled ? '' : 'hidden';
  const mode = ch.audio_mode || 'usb';
  const currentBwHz = ch.bandwidth_hz || 0;
  const cfg = bwCfg(mode);
  const bwVal = currentBwHz > 0 ? Math.max(cfg.min, Math.min(cfg.max, currentBwHz)) : cfg.default;

  const bellOn = isBellEnabled(ch.label);
  const bellHtml = enabled
    ? `<button class="ch-bell-btn${bellOn ? ' ch-bell-on' : ''}" title="${bellOn ? 'Alert on: click to disable' : 'Alert off: click to enable'}" aria-label="VOX alert bell">${bellOn ? '🔔' : '🔕'}</button>`
    : '';

  // Determine current rec-mode for the select
  const sched = ch.schedule || {};
  const schedEnabled = !!sched.enabled;
  let recModeVal = 'continuous';
  if (schedEnabled) recModeVal = 'scheduled';
  else if (enabled) recModeVal = 'smart';

  // Per-channel quota (MB) — 0 means use overall limit
  const channelQuotaMB = (quotaState.channels && quotaState.channels[ch.label]) || 0;

  return `
    <div class="sr-settings-wrap">
      <div class="sr-settings-toggle-row">
        <button class="sr-settings-toggle" title="Channel settings">⚙ Settings</button>
        ${bellHtml}
      </div>
      <div class="sr-settings-panel hidden">
        <div class="bw-row">
          <label class="bw-label">Bandwidth
            <span class="bw-value sr-bw-value">${fmtBW(bwVal)}</span>
          </label>
          <input type="range" class="bw-slider sr-bw-slider"
                 min="${cfg.min}" max="${cfg.max}" step="${cfg.step}" value="${bwVal}" />
        </div>
        <label class="sr-quota-label">Max storage for this channel (MB, 0 = use overall limit)
          <input type="number" class="ch-quota-input" value="${channelQuotaMB}" min="0" step="1" placeholder="0" />
        </label>
        <label class="sr-mode-label">Recording mode
          <select class="sr-mode-select">
            <option value="continuous"${recModeVal === 'continuous' ? ' selected' : ''}>Continuous</option>
            <option value="smart"${recModeVal === 'smart' ? ' selected' : ''}>Signal Activated (VOX)</option>
            <option value="scheduled"${recModeVal === 'scheduled' ? ' selected' : ''}>Scheduled</option>
          </select>
        </label>
        <div class="sr-thresh-fields ${recModeVal === 'smart' ? '' : 'hidden'}">
          <div class="smart-record-grid">
            <label>Start SNR threshold (dB)
              <input type="number" class="sr-start-thresh" value="${startThresh}" step="0.5" />
            </label>
            <label>Start hold (seconds)
              <input type="number" class="sr-start-hold" value="${startHold}" min="0.1" step="0.1" />
            </label>
            <label>Stop SNR threshold (dB)
              <input type="number" class="sr-stop-thresh" value="${stopThresh}" step="0.5" />
            </label>
            <label>Stop hold (seconds)
              <input type="number" class="sr-stop-hold" value="${stopHold}" min="0.1" step="0.1" />
            </label>
            <label>Max recording length (minutes, 0 = unlimited)
              <input type="number" class="sr-max-record-mins" value="${maxRecordMins}" min="0" step="0.5" />
            </label>
          </div>
          <p class="smart-record-hint">
            Recording starts when SNR ≥ start threshold for ≥ start hold seconds.<br>
            Recording stops when SNR &lt; stop threshold for ≥ stop hold seconds.<br>
            If max length &gt; 0, recording stops when that duration is reached and will not restart until the SNR drops below the stop threshold and rises again above the start threshold.
          </p>
        </div>
        <div class="sched-fields ${recModeVal === 'scheduled' ? '' : 'hidden'}">
          ${buildScheduleEditor(sched)}
        </div>
        <button class="sr-save-btn">💾 Save</button>
        <span class="sr-save-status"></span>
      </div>
    </div>`;
}

// ── Schedule editor helpers ───────────────────────────────────────────────────

const WEEKDAY_NAMES = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

// Build the HTML for the schedule editor (used in both the card panel and the
// add-channel form).  sched is the current ScheduleConfig (may be empty {}).
const TIMEZONES = [
  'UTC',
  'Europe/London','Europe/Paris','Europe/Berlin','Europe/Madrid','Europe/Rome',
  'Europe/Amsterdam','Europe/Brussels','Europe/Zurich','Europe/Stockholm',
  'Europe/Oslo','Europe/Copenhagen','Europe/Helsinki','Europe/Warsaw',
  'Europe/Prague','Europe/Vienna','Europe/Budapest','Europe/Bucharest',
  'Europe/Athens','Europe/Istanbul','Europe/Moscow',
  'America/New_York','America/Chicago','America/Denver','America/Los_Angeles',
  'America/Anchorage','America/Honolulu','America/Toronto','America/Vancouver',
  'America/Sao_Paulo','America/Argentina/Buenos_Aires','America/Mexico_City',
  'Asia/Tokyo','Asia/Shanghai','Asia/Hong_Kong','Asia/Singapore','Asia/Seoul',
  'Asia/Kolkata','Asia/Dubai','Asia/Riyadh','Asia/Bangkok','Asia/Jakarta',
  'Asia/Karachi','Asia/Dhaka','Asia/Taipei',
  'Australia/Sydney','Australia/Melbourne','Australia/Brisbane','Australia/Perth',
  'Pacific/Auckland','Pacific/Fiji',
  'Africa/Cairo','Africa/Johannesburg','Africa/Lagos','Africa/Nairobi',
];

function buildScheduleEditor(sched) {
  const tz = sched.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  const rules = (sched.rules && sched.rules.length > 0) ? sched.rules : [defaultRule()];
  const rulesHtml = rules.map((r, i) => buildRuleRow(r, i)).join('');
  // Build timezone <select> options; if the saved tz isn't in the list, add it first
  const tzList = TIMEZONES.includes(tz) ? TIMEZONES : [tz, ...TIMEZONES];
  const tzOptions = tzList.map(z =>
    `<option value="${escHtml(z)}"${z === tz ? ' selected' : ''}>${escHtml(z)}</option>`
  ).join('');
  return `
    <div class="sched-editor">
      <div class="sched-tz-row">
        <label class="sched-label">Timezone
          <select class="sched-tz">${tzOptions}</select>
        </label>
      </div>
      <div class="sched-rules-list">${rulesHtml}</div>
      <button type="button" class="sched-add-rule-btn">+ Add rule</button>
      <p class="smart-record-hint">
        Recording is active when <em>any</em> rule matches the current time.<br>
        Leave weekdays empty for every day. Midnight-spanning windows (e.g. 22:00→02:00) are supported.
      </p>
    </div>`;
}

function defaultRule() {
  return { weekdays: [], start_time: '08:00', stop_time: '18:00', from_date: '', to_date: '' };
}

function escHtml(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function buildRuleRow(rule, idx) {
  const wdChecks = WEEKDAY_NAMES.map((name, d) => {
    const checked = (!rule.weekdays || rule.weekdays.length === 0 || rule.weekdays.includes(d)) ? '' : '';
    const isChecked = (rule.weekdays && rule.weekdays.length > 0 && rule.weekdays.includes(d)) ? ' checked' : '';
    return `<label class="sched-wd-label"><input type="checkbox" class="sched-wd" value="${d}"${isChecked}>${name}</label>`;
  }).join('');

  return `
    <div class="sched-rule-row" data-rule-idx="${idx}">
      <div class="sched-rule-header">
        <span class="sched-rule-num">Rule ${idx + 1}</span>
        <button type="button" class="sched-remove-rule-btn" title="Remove rule">✕</button>
      </div>
      <div class="sched-weekdays">${wdChecks}</div>
      <div class="sched-time-row">
        <label class="sched-label">Start
          <input type="time" class="sched-start-time" value="${escHtml(rule.start_time || '08:00')}" />
        </label>
        <label class="sched-label">Stop
          <input type="time" class="sched-stop-time" value="${escHtml(rule.stop_time || '18:00')}" />
        </label>
      </div>
      <div class="sched-date-row">
        <label class="sched-label">From date (optional)
          <input type="date" class="sched-from-date" value="${escHtml(rule.from_date || '')}" />
        </label>
        <label class="sched-label">To date (optional)
          <input type="date" class="sched-to-date" value="${escHtml(rule.to_date || '')}" />
        </label>
      </div>
    </div>`;
}

// Read the schedule config from a .sched-editor element.
function readScheduleFromEditor(editorEl) {
  const tz = editorEl.querySelector('.sched-tz').value.trim() || 'UTC';
  const ruleEls = editorEl.querySelectorAll('.sched-rule-row');
  const rules = [];
  ruleEls.forEach(row => {
    const startTime = row.querySelector('.sched-start-time').value.trim();
    const stopTime  = row.querySelector('.sched-stop-time').value.trim();
    const fromDate  = row.querySelector('.sched-from-date').value.trim();
    const toDate    = row.querySelector('.sched-to-date').value.trim();
    const wdBoxes   = row.querySelectorAll('.sched-wd:checked');
    const weekdays  = Array.from(wdBoxes).map(cb => parseInt(cb.value, 10));
    const rule = { start_time: startTime, stop_time: stopTime };
    if (weekdays.length > 0) rule.weekdays = weekdays;
    if (fromDate) rule.from_date = fromDate;
    if (toDate)   rule.to_date   = toDate;
    rules.push(rule);
  });
  return { enabled: true, timezone: tz, rules };
}

// Wire up dynamic behaviour for a .sched-editor inside a given container.
function wireScheduleEditor(container) {
  const editor = container.querySelector('.sched-editor');
  if (!editor) return;

  // "Add rule" button
  const addBtn = editor.querySelector('.sched-add-rule-btn');
  if (addBtn) {
    addBtn.onclick = () => {
      const list = editor.querySelector('.sched-rules-list');
      const idx  = list.querySelectorAll('.sched-rule-row').length;
      const tmp  = document.createElement('div');
      tmp.innerHTML = buildRuleRow(defaultRule(), idx);
      const row = tmp.firstElementChild;
      list.appendChild(row);
      wireRuleRow(row, editor);
      renumberRules(editor);
    };
  }

  // Wire existing rule rows
  editor.querySelectorAll('.sched-rule-row').forEach(row => wireRuleRow(row, editor));
}

function wireRuleRow(row, editor) {
  const removeBtn = row.querySelector('.sched-remove-rule-btn');
  if (removeBtn) {
    removeBtn.onclick = () => {
      row.remove();
      renumberRules(editor);
    };
  }
}

function renumberRules(editor) {
  editor.querySelectorAll('.sched-rule-row').forEach((row, i) => {
    row.dataset.ruleIdx = i;
    const num = row.querySelector('.sched-rule-num');
    if (num) num.textContent = `Rule ${i + 1}`;
  });
}

// Save smart-record / schedule settings (and bandwidth) from a channel card.
function saveSmartRecord(card, label) {
  const modeSelect   = card.querySelector('.sr-mode-select');
  const statusEl     = card.querySelector('.sr-save-status');
  const modeVal      = modeSelect ? modeSelect.value : 'continuous';

  let srConfig;
  let schedConfig = { enabled: false };

  if (modeVal === 'smart') {
    const startThresh   = parseFloat(card.querySelector('.sr-start-thresh').value);
    const startHold     = parseFloat(card.querySelector('.sr-start-hold').value);
    const stopThresh    = parseFloat(card.querySelector('.sr-stop-thresh').value);
    const stopHold      = parseFloat(card.querySelector('.sr-stop-hold').value);
    const maxRecordMins = parseFloat(card.querySelector('.sr-max-record-mins').value) || 0;
    if ([startThresh, startHold, stopThresh, stopHold].some(isNaN)) {
      if (statusEl) { statusEl.textContent = '✗ Invalid values'; statusEl.className = 'sr-save-status error'; }
      return;
    }
    srConfig = {
      enabled: true,
      start_thresh_db:  startThresh,
      start_hold_sec:   startHold,
      stop_thresh_db:   stopThresh,
      stop_hold_sec:    stopHold,
      max_record_mins:  maxRecordMins,
    };
  } else if (modeVal === 'scheduled') {
    srConfig = { enabled: false, start_thresh_db: 0, start_hold_sec: 0, stop_thresh_db: 0, stop_hold_sec: 0 };
    const editorEl = card.querySelector('.sched-editor');
    if (!editorEl) {
      if (statusEl) { statusEl.textContent = '✗ Schedule editor not found'; statusEl.className = 'sr-save-status error'; }
      return;
    }
    schedConfig = readScheduleFromEditor(editorEl);
    if (!schedConfig.rules || schedConfig.rules.length === 0) {
      if (statusEl) { statusEl.textContent = '✗ Add at least one rule'; statusEl.className = 'sr-save-status error'; }
      return;
    }
  } else {
    srConfig = { enabled: false, start_thresh_db: 0, start_hold_sec: 0, stop_thresh_db: 0, stop_hold_sec: 0 };
  }

  // Also read bandwidth from the card slider.
  const bwSlider = card.querySelector('.sr-bw-slider');
  const bwHz = bwSlider ? parseInt(bwSlider.value, 10) : null;

  // Read per-channel quota (MB).
  const quotaInput = card.querySelector('.ch-quota-input');
  const maxMB = quotaInput ? parseInt(quotaInput.value, 10) : null;

  const patch = { smart_record: srConfig, schedule: schedConfig };
  if (bwHz != null && !isNaN(bwHz)) patch.bandwidth_hz = bwHz;
  if (maxMB != null && !isNaN(maxMB) && maxMB >= 0) patch.max_mb = maxMB;

  api('/api/channels/' + encodeURIComponent(label), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  }).then(() => {
    if (statusEl) { statusEl.textContent = '✓ Saved'; statusEl.className = 'sr-save-status ok'; }
    setTimeout(() => { if (statusEl) statusEl.textContent = ''; }, 2000);
    loadChannels();
  }).catch(e => {
    if (statusEl) { statusEl.textContent = '✗ ' + e; statusEl.className = 'sr-save-status error'; }
  });
}

// ── Real-time WebSocket live audio (Web Audio API) ───────────────────────────
//
// Each channel card gets a _livePlayer object when active:
//   { ws, audioCtx, nextTime, sampleRate }
// PCM arrives as S16LE binary frames; we convert to Float32 and schedule
// AudioBufferSourceNodes with a small jitter buffer.

const JITTER_SECS = 0.2; // seconds of scheduling look-ahead

function toggleLivePlayer(btn, label) {
  const card = btn.closest('.channel-card');

  if (card._livePlayer) {
    // Stop this player.
    stopLivePlayer(card);
    btn.textContent = '▶ Listen live';
    return;
  }

  // Stop any other live player that is currently active.
  document.querySelectorAll('.channel-card').forEach(otherCard => {
    if (otherCard !== card && otherCard._livePlayer) {
      stopLivePlayer(otherCard);
      const otherBtn = otherCard.querySelector('.ch-live-btn');
      if (otherBtn) otherBtn.textContent = '▶ Listen live';
    }
  });

  btn.textContent = '⏹ Stop';

  const wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const wsUrl = `${wsProto}//${location.host}${BASE}/api/live/${encodeURIComponent(label)}/ws`;
  const ws = new WebSocket(wsUrl);
  ws.binaryType = 'arraybuffer';

  let audioCtx = null;
  let sampleRate = 8000;
  let nextTime = 0;        // AudioContext time to schedule next chunk
  let infoReceived = false;

  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') {
      // First frame: stream info JSON
      try {
        const info = JSON.parse(ev.data);
        sampleRate = info.sample_rate || 8000;
      } catch (_) {}
      audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate });
      nextTime = audioCtx.currentTime + JITTER_SECS;
      infoReceived = true;
      return;
    }
    if (!infoReceived || !audioCtx) return;

    // Binary frame: raw S16LE mono PCM
    const s16 = new Int16Array(ev.data);
    const float32 = new Float32Array(s16.length);
    for (let i = 0; i < s16.length; i++) {
      float32[i] = s16[i] / 32768.0;
    }

    const buf = audioCtx.createBuffer(1, float32.length, sampleRate);
    buf.copyToChannel(float32, 0);

    const src = audioCtx.createBufferSource();
    src.buffer = buf;
    src.connect(audioCtx.destination);

    // Schedule: if we've fallen behind, reset to now + jitter
    const now = audioCtx.currentTime;
    if (nextTime < now) nextTime = now + JITTER_SECS;
    src.start(nextTime);
    nextTime += buf.duration;
  };

  ws.onerror = () => {
    stopLivePlayer(card);
    btn.textContent = '▶ Listen live';
  };
  ws.onclose = () => {
    if (card._livePlayer) {
      stopLivePlayer(card);
      btn.textContent = '▶ Listen live';
    }
  };

  card._livePlayer = { ws, get audioCtx() { return audioCtx; } };
}

function stopLivePlayer(card) {
  const p = card._livePlayer;
  if (!p) return;
  card._livePlayer = null;
  try { p.ws.close(); } catch (_) {}
  try { if (p.audioCtx) p.audioCtx.close(); } catch (_) {}
}

// ── Add / Remove channels ─────────────────────────────────────────────────────

function showAddChannelForm() {
  document.getElementById('add-channel-form').classList.remove('hidden');
  document.getElementById('add-name').focus();
  // Initialise bandwidth slider for the currently-selected mode.
  const mode   = document.getElementById('add-mode').value;
  const slider = document.getElementById('add-bw-slider');
  const label  = document.getElementById('add-bw-value');
  if (slider && label) configureBWSlider(slider, label, mode, 0);
  // Inject schedule editor into the mount point if not already done.
  const mount = document.getElementById('add-sched-editor-mount');
  if (mount && !mount._schedInjected) {
    mount.innerHTML = buildScheduleEditor({});
    mount._schedInjected = true;
    // Wire the editor (add/remove rule buttons) — parent is add-sched-fields
    const schedFields = document.getElementById('add-sched-fields');
    if (schedFields) {
      wireScheduleEditor(schedFields);
      schedFields._wired = true;
    }
  }
}

function hideAddChannelForm() {
  const form = document.getElementById('add-channel-form');
  if (form) form.classList.add('hidden');
  const err = document.getElementById('add-channel-error');
  if (err) err.classList.add('hidden');
}

// Reconfigure bandwidth slider when mode changes in the add-channel form.
document.getElementById('add-mode').addEventListener('change', function () {
  const slider = document.getElementById('add-bw-slider');
  const label  = document.getElementById('add-bw-value');
  if (slider && label) configureBWSlider(slider, label, this.value, 0);
});

// Show/hide the smart-record / schedule fields in the add-channel form.
document.getElementById('add-rec-mode').addEventListener('change', function () {
  const smartFields = document.getElementById('add-smart-fields');
  const schedFields = document.getElementById('add-sched-fields');
  smartFields.classList.toggle('hidden', this.value !== 'smart');
  if (schedFields) schedFields.classList.toggle('hidden', this.value !== 'scheduled');
  // Wire schedule editor when it becomes visible for the first time
  if (this.value === 'scheduled' && schedFields && !schedFields._wired) {
    wireScheduleEditor(schedFields);
    schedFields._wired = true;
  }
});

function doAddChannel() {
  const nameRaw = document.getElementById('add-name').value.trim();
  const freqRaw = document.getElementById('add-freq').value.trim();
  const mode    = document.getElementById('add-mode').value;
  const recMode = document.getElementById('add-rec-mode').value;
  const errEl   = document.getElementById('add-channel-error');

  // Input is in kHz — convert to Hz (round to nearest Hz).
  const freqKHz = parseFloat(freqRaw);
  const freqHz  = Math.round(freqKHz * 1000);
  if (!freqKHz || freqHz < 10000 || freqHz > 30000000) {
    errEl.textContent = 'Frequency must be between 10 kHz and 30 MHz.';
    errEl.classList.remove('hidden');
    return;
  }

  // Read bandwidth from slider.
  const bwSlider = document.getElementById('add-bw-slider');
  const bwHz = bwSlider ? parseInt(bwSlider.value, 10) : 0;

  const payload = { freq_hz: freqHz, mode };
  if (nameRaw) payload.name = nameRaw;
  if (bwHz > 0) payload.bandwidth_hz = bwHz;

  if (recMode === 'smart') {
    const startThresh   = parseFloat(document.getElementById('add-sr-start-thresh').value);
    const startHold     = parseFloat(document.getElementById('add-sr-start-hold').value);
    const stopThresh    = parseFloat(document.getElementById('add-sr-stop-thresh').value);
    const stopHold      = parseFloat(document.getElementById('add-sr-stop-hold').value);
    const maxRecordMins = parseFloat(document.getElementById('add-sr-max-record-mins').value) || 0;
    if (isNaN(startThresh) || isNaN(startHold) || isNaN(stopThresh) || isNaN(stopHold)) {
      errEl.textContent = 'All smart record fields must be valid numbers.';
      errEl.classList.remove('hidden');
      return;
    }
    payload.smart_record = {
      enabled: true,
      start_thresh_db:  startThresh,
      start_hold_sec:   startHold,
      stop_thresh_db:   stopThresh,
      stop_hold_sec:    stopHold,
      max_record_mins:  maxRecordMins,
    };
  } else if (recMode === 'scheduled') {
    const schedFieldsEl = document.getElementById('add-sched-fields');
    const editorEl = schedFieldsEl && schedFieldsEl.querySelector('.sched-editor');
    if (!editorEl) {
      errEl.textContent = 'Schedule editor not found.';
      errEl.classList.remove('hidden');
      return;
    }
    const schedConfig = readScheduleFromEditor(editorEl);
    if (!schedConfig.rules || schedConfig.rules.length === 0) {
      errEl.textContent = 'Add at least one schedule rule.';
      errEl.classList.remove('hidden');
      return;
    }
    payload.schedule = schedConfig;
  }

  api('/api/channels', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  }).then(() => {
    document.getElementById('add-name').value = '';
    document.getElementById('add-freq').value = '';
    document.getElementById('add-rec-mode').value = 'continuous';
    document.getElementById('add-smart-fields').classList.add('hidden');
    const sf = document.getElementById('add-sched-fields');
    if (sf) sf.classList.add('hidden');
    errEl.classList.add('hidden');
    hideAddChannelForm();
    loadChannels();
  }).catch(e => {
    errEl.textContent = 'Error: ' + e;
    errEl.classList.remove('hidden');
  });
}

function removeChannel(label) {
  if (!confirm(`Remove channel "${label}"? Any active recording will be finalised.`)) return;
  fetch(BASE + '/api/channels/' + encodeURIComponent(label), { method: 'DELETE' })
    .then(r => {
      if (!r.ok) return r.text().then(t => Promise.reject(t));
      // Card removed via SSE channel_removed; also do a full reload for safety
      loadChannels();
    })
    .catch(e => alert('Remove failed: ' + e));
}

function showRenameChannel(btn, label) {
  const card = btn.closest('.channel-card');
  card.querySelector('.ch-rename-form').classList.remove('hidden');
  card.querySelector('.rename-ch-btn').classList.add('hidden');
  const input = card.querySelector('.ch-rename-input');
  input.value = label;
  input.focus();
  input.select();
}

function hideRenameChannel(btn) {
  const card = btn.closest('.channel-card');
  card.querySelector('.ch-rename-form').classList.add('hidden');
  card.querySelector('.rename-ch-btn').classList.remove('hidden');
}

function doRenameChannel(btn) {
  const card = btn.closest('.channel-card');
  const oldLabel = card.dataset.label;
  const newName = card.querySelector('.ch-rename-input').value.trim();
  if (!newName || newName === oldLabel) { hideRenameChannel(btn); return; }
  api('/api/channels/' + encodeURIComponent(oldLabel), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: newName }),
  }).then(() => {
    loadChannels();
    loadRecordings();
  }).catch(e => alert('Rename failed: ' + e));
}

// Wire up add-channel form buttons (elements exist in DOM at parse time)
document.getElementById('add-channel-btn').onclick = showAddChannelForm;
document.getElementById('add-channel-submit').onclick = doAddChannel;
document.getElementById('add-channel-cancel').onclick = hideAddChannelForm;
document.getElementById('add-freq').addEventListener('keydown', e => {
  if (e.key === 'Enter') doAddChannel();
});

// ── Sessions / Recordings ─────────────────────────────────────────────────────

function loadRecordings() {
  const params = new URLSearchParams({
    limit: PAGE_SIZE,
    offset: state.offset,
    group_by: 'channel',
    date: state.date, // YYYY-MM-DD; always a specific day (never empty)
  });
  if (state.channelId) params.set('channel_id', state.channelId);

  // Fetch completed sessions and active (in-progress) segments in parallel.
  Promise.all([
    api('/api/sessions?' + params),
    api('/api/active').catch(() => []),
  ]).then(([data, active]) => {
    state.total = data.total || 0;
    let sessions = data.sessions || [];

    // Exclude live (currently-recording) segments that started on a different
    // day — they belong to today and should only appear when today is selected.
    const activeFiltered = (active || []).filter(seg => {
      const segDate = new Date(seg.started_at).toISOString().slice(0, 10);
      return segDate === state.date;
    });

    // Merge active segments into their channel-grouped card (or create a synthetic one).
    // Track which channel IDs already have a live segment merged so we don't
    // create a duplicate card for the same channel.
    const mergedChannels = new Set();
    activeFiltered.forEach(seg => {
      if (state.channelId && seg.channel_id !== state.channelId) return;
      // Deduplicate by channel_id (preferred) or label (fallback for old records).
      const dedupeKey = seg.channel_id || seg.label;
      if (mergedChannels.has(dedupeKey)) return;
      mergedChannels.add(dedupeKey);

      // Find existing channel-grouped card by channel_id first, then by label.
      let sess = seg.channel_id
        ? sessions.find(s => s.channel_id === seg.channel_id)
        : null;
      if (!sess) {
        sess = sessions.find(s => s.label === seg.label);
      }
      if (!sess) {
        // No existing session at all — create a synthetic one.
        sess = {
          session_id: seg.session_id,
          channel_id: seg.channel_id || '',
          label: seg.label,
          freq_hz: seg.freq_hz,
          audio_mode: seg.audio_mode,
          started_at: seg.started_at,
          saved_at: seg.started_at,
          duration_sec: 0,
          segment_count: 0,
          snr: {},
          segments: [],
          _live: true,
        };
        sessions.unshift(sess);
      }
      // Add the live segment as a synthetic segment record.
      // Use /api/active/{label}/stream which streams the in-progress WAV file.
      const liveSeg = {
        id: 'live_' + seg.label,
        session_id: seg.session_id,
        segment_index: seg.segment_index,
        label: seg.label,
        freq_hz: seg.freq_hz,
        audio_mode: seg.audio_mode,
        started_at: seg.started_at,
        duration_sec: seg.duration_sec,
        snr: seg.snr || {},
        filename: null,
        _live: true,
        _liveStreamUrl: `${BASE}/api/active/${encodeURIComponent(seg.label)}/stream`,
      };
      // Prepend live segment (it's the current one); remove any stale live entry.
      sess.segments = [liveSeg, ...sess.segments.filter(s => !s._live)];
      sess.segment_count = sess.segments.length;
      // Add only the live segment's duration (don't re-add completed duration).
      sess.duration_sec = (sess.duration_sec || 0) + seg.duration_sec;
      sess._live = true;
      // Propagate live SNR to session header if the session has no SNR yet.
      if (seg.snr && seg.snr.count > 0 && !(sess.snr && sess.snr.count > 0)) {
        sess.snr = seg.snr;
      }
    });

    renderSessions(sessions);
    renderPagination();
    // Sync the label filter: keep labels from active channels AND from the
    // current sessions list so deleted/inactive channels with recordings
    // remain selectable.
    // Build {id, label} pairs for active channel cards and for sessions returned
    // by the API, then sync the filter dropdown to their union.
    const activeChannels = Array.from(
      document.querySelectorAll('.channel-card[data-label]')
    ).map(c => ({ id: c.dataset.channelId || c.dataset.label, label: c.dataset.label }));
    const sessionChannels = sessions
      .filter(s => s.label)
      .map(s => ({ id: s.channel_id || s.label, label: s.label }));
    syncLabelFilter(activeChannels, sessionChannels);
  }).catch(e => {
    document.getElementById('recordings-list').innerHTML =
      `<div class="empty">Error loading sessions: ${e}</div>`;
  });
}

// Returns true if a session card should be preserved during a refresh.
// Preserves cards that:
//   - have audio currently playing or paused-with-position (user has seeked into it)
//   - have a live player active
// NOTE: the .session-player panel is always visible (not toggled hidden), so we
// deliberately do NOT treat its visibility as a signal of user interaction.
function sessionCardIsActive(card) {
  if (!card) return false;
  // Live audio player active
  if (card._livePlayer) return true;
  // Session-level audio has been loaded (src set and metadata available)
  const sessAudio = card.querySelector('.sess-audio');
  if (sessAudio && sessAudio.src && sessAudio.readyState >= 1) return true;
  // Any segment-level audio is playing or loaded
  const segAudios = card.querySelectorAll('.seg-audio');
  for (const a of segAudios) {
    if (a.src && !a.paused) return true;
  }
  return false;
}

function renderSessions(sessions) {
  const list = document.getElementById('recordings-list');
  if (!sessions.length) {
    list.innerHTML = '<div class="empty">No recordings yet.</div>';
    _sharedWindowMs = null;
    return;
  }

  // Card key: prefer channel_id UUID (stable across renames/restarts), fall back to session_id.
  const cardKey = ss => ss.channel_id || ss.session_id;

  // Build set of card keys in the new data.
  const newKeys = new Set(sessions.map(cardKey));

  // Remove cards for sessions that are no longer in the current filtered view.
  // We always remove cards whose key is absent from the new data — even if the
  // card has audio loaded — because the user has changed the date/label filter
  // and stale cards from a different day must not persist.
  let anyRemoved = false;
  Array.from(list.querySelectorAll('.session-card')).forEach(card => {
    const key = card.dataset.channelId || card.dataset.sessionId;
    if (!newKeys.has(key)) {
      // Stop any audio playing on this card before removing it.
      card.querySelectorAll('audio').forEach(a => { try { a.pause(); a.src = ''; } catch (_) {} });
      stopPlayheadLoop(card);
      card.remove();
      anyRemoved = true;
    }
  });
  if (anyRemoved) recomputeSharedTimeWindow();

  // Add or update cards. Preserve existing cards that are actively playing.
  sessions.forEach((ss, idx) => {
    const key = cardKey(ss);
    const existing = list.querySelector(`.session-card[data-channel-id="${key}"]`)
      || list.querySelector(`.session-card[data-session-id="${key}"]`);
    if (existing && sessionCardIsActive(existing)) {
      // Card is active — preserve audio state but update segment list and metadata.
      // Update _segments so new completed segments (and the live segment) are
      // available for seek/step. Live segment has _liveStreamUrl so it's playable.
      const updatedSegs = (ss.segments || [])
        .slice()
        .sort((a, b) => new Date(a.started_at) - new Date(b.started_at));
      existing._segments = updatedSegs;
      // Update live badge and segment count in header without rebuilding the card.
      const countEl = existing.querySelector('.sess-count');
      if (countEl) countEl.textContent = `${ss.segment_count} segment${ss.segment_count !== 1 ? 's' : ''}`;
      const durEl = existing.querySelector('.sess-dur');
      if (durEl) durEl.textContent = fmtDuration(ss.duration_sec);
      // Ensure correct position in list.
      const cards = Array.from(list.querySelectorAll('.session-card'));
      const currentIdx = cards.indexOf(existing);
      if (currentIdx !== idx) {
        const refCard = list.querySelectorAll('.session-card')[idx];
        if (refCard && refCard !== existing) {
          list.insertBefore(existing, refCard);
        }
      }
      return;
    }
    if (existing) {
      // Card exists but not playing — replace it with fresh render.
      const newCard = document.createElement('div');
      renderSessionCard(ss, newCard);
      const inner = newCard.firstElementChild;
      if (inner) {
        list.insertBefore(inner, existing);
        existing.remove();
      }
    } else {
      // New session — insert at correct position.
      const cards = list.querySelectorAll('.session-card');
      const refCard = cards[idx] || null;
      const tmp = document.createElement('div');
      renderSessionCard(ss, tmp);
      const inner = tmp.firstElementChild;
      if (inner) {
        list.insertBefore(inner, refCard);
      }
    }
  });
}

function renderSessionCard(ss, container) {
  const card = document.createElement('div');
  card.className = 'session-card' + (ss._live ? ' session-live' : '');
  // Use channel_id as the stable card key; fall back to session_id for old records.
  const cardKey = ss.channel_id || ss.session_id;
  card.dataset.channelId = cardKey;
  card.dataset.sessionId = ss.session_id; // keep for backward compat

  const liveLabel = ss._live ? '<span class="live-badge">● LIVE</span>' : '';
  // Stream/delete/telemetry all use the card key (channel UUID when available).
  const streamUrl = `${BASE}/api/sessions/${cardKey}/stream`;
  const deleteBtn = (state.authed && !ss._live)
    ? `<button class="danger-btn" onclick="deleteSession('${cardKey}')">🗑 Delete all</button>`
    : '';
  // Show Download All for completed sessions, and also for live sessions that
  // already have at least one completed segment on disk (the stream endpoint
  // serves those; the currently-recording segment is excluded).
  const completedSegCount = (ss.segments || []).filter(s => !s._live).length;
  const downloadAllWavName = `${ss.label}_${cardKey.slice(0,8)}.wav`;
  const downloadBtn = (!ss._live || completedSegCount > 0)
    ? `<a href="${streamUrl}" download="${downloadAllWavName}">⬇ Download All</a>`
    : '';

  // Build datetime-local default value from session start
  const sessStart = new Date(ss.started_at);
  const sessEnd   = ss._live ? new Date() : new Date(ss.saved_at);
  const dtMin = toDatetimeLocal(sessStart);
  const dtMax = toDatetimeLocal(sessEnd);
  const dtDef = dtMin;

  card.innerHTML = `
    <div class="session-header" onclick="toggleSession(this)">
      <span class="sess-toggle">▶</span>
      ${liveLabel}
      <span class="sess-label">${ss.label}</span>
      <span class="sess-freq">${fmtFreq(ss.freq_hz)}</span>
      <span class="sess-mode">${(ss.audio_mode || '').toUpperCase()}</span>
      <span class="sess-time">${fmtDate(ss.started_at)}</span>
      <span class="sess-dur">${fmtDuration(ss.duration_sec)}</span>
      <span class="sess-snr">SNR: ${fmtSNR(ss.snr)}</span>
      <span class="sess-count">${ss.segment_count} segment${ss.segment_count !== 1 ? 's' : ''}</span>
    </div>
    <div class="session-actions">
      ${downloadBtn}
      <div class="seg-dl-wrap sess-dl-seg-wrap hidden">
        <button class="seg-dl-main" onclick="toggleSegDlMenu(this)">⬇ Download Segment ▾</button>
        <div class="seg-dl-menu hidden">
          <a href="#" download="" class="sess-dl-seg-wav">WAV</a>
          <button class="sess-dl-seg-mp3" onclick="downloadSegmentMp3(this.dataset.segId, this.dataset.wavFilename)">MP3</button>
        </div>
      </div>
      ${deleteBtn}
    </div>
    <div class="session-player">
      <div class="snr-timeline-wrap">
        <canvas class="snr-timeline" height="72" title="Click to seek to that time"></canvas>
        <div class="snr-timeline-empty hidden">No SNR data yet</div>
      </div>
      <div class="sess-seek-bar">
        <label class="sess-seek-label">Jump to time:</label>
        <input type="datetime-local" class="sess-seek-input"
               min="${dtMin}" max="${dtMax}" value="${dtDef}" step="1" />
        <button class="sess-seek-btn" onclick="seekSessionTo(this)">▶ Go</button>
        <span class="sess-now-playing"></span>
      </div>
      <div class="sess-player-controls">
        <button class="sess-prev-btn" onclick="sessStepSegment(this,-1)" disabled>⏮ Prev</button>
        <audio controls preload="none" class="sess-audio"></audio>
        <button class="sess-next-btn" onclick="sessStepSegment(this,+1)" disabled>Next ⏭</button>
      </div>
    </div>
    <div class="session-segments hidden"></div>`;

  // Store segments array on the card for the timeline player to use.
  // Include the live segment (it has _liveStreamUrl so sessLoadSegment can play it).
  // Sort by wall-clock start time — segment_index resets to 0 each session so
  // it cannot be used as a global ordering key in the channel-grouped view.
  card._segments = (ss.segments || [])
    .slice()
    .sort((a, b) => new Date(a.started_at) - new Date(b.started_at));

  // Wire seeked/play listeners on the session audio element so the playhead
  // loop restarts after a native seek-bar drag or play-after-pause.
  const sessAudio = card.querySelector('.sess-audio');
  if (sessAudio) {
    sessAudio.addEventListener('seeked', () => {
      if (!sessAudio.paused) startPlayheadLoop(card);
    });
    sessAudio.addEventListener('play', () => startPlayheadLoop(card));
  }

  // Populate segments
  const segContainer = card.querySelector('.session-segments');
  (ss.segments || []).forEach(seg => {
    const row = document.createElement('div');
    row.className = 'seg-row' + (seg._live ? ' seg-live' : '');
    row.dataset.id = seg.id;

    if (seg._live) {
      // Live (in-progress) segment row — show listen button, no download/delete.
      const liveStreamUrl = `${BASE}/api/active/${encodeURIComponent(seg.label)}/stream`;
      row.innerHTML = `
        <span class="seg-idx live-badge">● LIVE</span>
        <span class="seg-time">${fmtDate(seg.started_at)}</span>
        <span class="seg-dur">${fmtDuration(seg.duration_sec)} so far</span>
        <span class="seg-snr"></span>
        <div class="seg-actions">
          <button class="play-seg-btn" onclick="toggleSegPlayer(this, '${liveStreamUrl}')">▶ Listen</button>
        </div>
        <div class="seg-player hidden">
          <audio controls preload="none" class="seg-audio"></audio>
        </div>`;
    } else {
      const dlUrl = `${BASE}/recordings/${encodeURIComponent(seg.filename)}`;
      const mp3Url = `${BASE}/api/recordings/${encodeURIComponent(seg.id)}/mp3`;
      const delBtn = state.authed
        ? `<button onclick="deleteSegment('${seg.id}')">🗑</button>`
        : '';
      row.innerHTML = `
        <span class="seg-idx">#${seg.segment_index + 1}</span>
        <span class="seg-time">${fmtDate(seg.started_at)}</span>
        <span class="seg-dur">${fmtDuration(seg.duration_sec)}</span>
        <span class="seg-snr">SNR: ${fmtSNR(seg.snr)}</span>
        <div class="seg-actions">
          <button class="play-seg-btn" onclick="toggleSegPlayer(this, '${dlUrl}')">▶</button>
          <div class="seg-dl-wrap">
            <button class="seg-dl-main" onclick="toggleSegDlMenu(this)">⬇ Download Segment ▾</button>
            <div class="seg-dl-menu hidden">
              <a href="${dlUrl}" download="${seg.filename}">WAV</a>
              <button onclick="downloadSegmentMp3('${seg.id}', '${seg.filename}')">MP3</button>
            </div>
          </div>
          ${delBtn}
        </div>
        <div class="seg-player hidden">
          <audio controls preload="none" class="seg-audio" src="${dlUrl}"></audio>
        </div>`;
    }
    segContainer.appendChild(row);
  });

  container.appendChild(card);

  // Load SNR timeline after card is in DOM (canvas needs layout width).
  // Use channel UUID (cardKey) so telemetry spans all sessions for this channel.
  loadSnrTimeline(card, cardKey);
}

function toggleSessionPlayer(btn, sessionId) {
  const card = btn.closest('.session-card');
  const playerDiv = card.querySelector('.session-player');
  const audio = playerDiv.querySelector('audio');
  const isHidden = playerDiv.classList.contains('hidden');
  playerDiv.classList.toggle('hidden', !isHidden);
  btn.textContent = isHidden ? '⏹ Stop' : '▶ Play all';
  if (isHidden) {
    audio.play().catch(() => {});
  } else {
    audio.pause();
    audio.currentTime = 0;
  }
}

// Toggle the per-segment download format dropdown.
// Closes any other open dropdown first.
function toggleSegDlMenu(btn) {
  const wrap = btn.closest('.seg-dl-wrap');
  const menu = wrap.querySelector('.seg-dl-menu');
  const isOpen = !menu.classList.contains('hidden');

  // Close all other open menus.
  document.querySelectorAll('.seg-dl-menu:not(.hidden)').forEach(m => {
    if (m !== menu) m.classList.add('hidden');
  });

  menu.classList.toggle('hidden', isOpen);

  // Close when clicking outside.
  if (!isOpen) {
    const close = e => {
      if (!wrap.contains(e.target)) {
        menu.classList.add('hidden');
        document.removeEventListener('click', close, true);
      }
    };
    // Use capture so the listener fires before the button's own click.
    setTimeout(() => document.addEventListener('click', close, true), 0);
  }
}

// Fetch the MP3 for a segment and trigger a browser download.
// Uses a temporary <a> element so the browser saves the file rather than
// navigating to it (fetch + createObjectURL gives us a proper filename).
function downloadSegmentMp3(segId, wavFilename) {
  // Close any open dropdown.
  document.querySelectorAll('.seg-dl-menu:not(.hidden)').forEach(m => m.classList.add('hidden'));

  const mp3Url = `${BASE}/api/recordings/${encodeURIComponent(segId)}/mp3`;
  const mp3Name = wavFilename.replace(/\.wav$/i, '.mp3').replace(/^.*[\\/]/, '');

  fetch(mp3Url)
    .then(r => {
      if (!r.ok) return r.text().then(t => Promise.reject(t));
      return r.blob();
    })
    .then(blob => {
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = mp3Name;
      document.body.appendChild(a);
      a.click();
      setTimeout(() => { URL.revokeObjectURL(url); a.remove(); }, 1000);
    })
    .catch(e => alert('MP3 download failed: ' + e));
}

function toggleSegPlayer(btn, url) {
  const row = btn.closest('.seg-row');
  const playerDiv = row.querySelector('.seg-player');
  const audio = playerDiv.querySelector('audio');
  const isHidden = playerDiv.classList.contains('hidden');

  // Pause any other playing segment in the same session
  const card = row.closest('.session-card');
  card.querySelectorAll('.seg-player:not(.hidden)').forEach(p => {
    if (p !== playerDiv) {
      p.querySelector('audio').pause();
      p.classList.add('hidden');
      p.closest('.seg-row').querySelector('.play-seg-btn').textContent = '▶';
    }
  });

  playerDiv.classList.toggle('hidden', !isHidden);
  btn.textContent = isHidden ? '⏹' : '▶';
  if (isHidden) {
    audio.play().catch(() => {});
  } else {
    audio.pause();
    audio.currentTime = 0;
  }
}

// ── Session timeline player ───────────────────────────────────────────────────

// ── SNR timeline canvas ───────────────────────────────────────────────────────
//
// Fetches /api/sessions/{id}/telemetry and draws an SNR bar chart on the
// canvas inside the session-player.  Clicking seeks the session player.
//
// All visible session cards share a common time window so their SNR graphs
// are time-aligned with each other.  The shared window is recomputed every
// time a card's telemetry loads or a card is removed.

// Shared time window across all session cards (milliseconds since epoch).
// null means "not yet computed / only one card".
let _sharedWindowMs = null; // { startMs, endMs } or null

// Recompute the shared time window from all cards that have loaded telemetry,
// then redraw every timeline so they all use the same axis.
function recomputeSharedTimeWindow() {
  const cards = Array.from(document.querySelectorAll('.session-card'));
  let minMs = Infinity;
  let maxMs = -Infinity;
  let count = 0;

  cards.forEach(card => {
    const tel = card._telemetry;
    if (!tel) return;
    const startMs = tel.startedAt.getTime();
    const endMs   = startMs + tel.durationSec * 1000;
    if (startMs < minMs) minMs = startMs;
    if (endMs   > maxMs) maxMs = endMs;
    count++;
  });

  if (count === 0) {
    _sharedWindowMs = null;
    return;
  }

  _sharedWindowMs = { startMs: minMs, endMs: maxMs };

  // Redraw every card's timeline with the new shared window.
  cards.forEach(card => {
    const tel    = card._telemetry;
    const canvas = card.querySelector('.snr-timeline');
    if (!tel || !canvas) return;
    drawSnrTimeline(canvas, tel.points, tel.startedAt, _sharedWindowMs.startMs, _sharedWindowMs.endMs);
    // Redraw playhead if audio is currently playing on this card.
    if (card._currentSegIdx != null) {
      const seg = card._segments && card._segments[card._currentSegIdx];
      const audio = card.querySelector('.sess-audio');
      if (seg && audio && !audio.paused) {
        const segOffsetFromStart = (new Date(seg.started_at).getTime() - _sharedWindowMs.startMs) / 1000;
        const windowSec = (_sharedWindowMs.endMs - _sharedWindowMs.startMs) / 1000;
        drawPlayhead(card, segOffsetFromStart + (audio.currentTime || 0), windowSec);
      }
    }
  });
}

function loadSnrTimeline(card, sessionId) {
  const canvas  = card.querySelector('.snr-timeline');
  const empty   = card.querySelector('.snr-timeline-empty');
  if (!canvas) return;

  // Pass the current date filter so the telemetry spans only the selected day.
  const dateParam = state.date !== undefined ? `?date=${encodeURIComponent(state.date)}` : '';
  api(`/api/sessions/${sessionId}/telemetry${dateParam}`).then(data => {
    // Sort by offset_sec so bars are drawn left-to-right regardless of server order.
    const points = (data.points || [])
      .filter(p => p.snr && p.snr.count > 0)
      .sort((a, b) => a.offset_sec - b.offset_sec);
    if (!points.length) {
      canvas.classList.add('hidden');
      if (empty) empty.classList.remove('hidden');
      // Still recompute in case this card was previously counted.
      recomputeSharedTimeWindow();
      return;
    }
    canvas.classList.remove('hidden');
    if (empty) empty.classList.add('hidden');

    // Compute the true wall-clock span from the actual segment timestamps.
    // Exclude live (in-progress) segments — they have started_at = now and
    // would inflate wallSpanSec to ~86400 s, breaking the shared time window.
    const startedAt = new Date(data.started_at);
    let wallSpanSec = data.duration_sec; // fallback: sum of audio durations
    const segs = (card._segments || []).filter(s => !s._live);
    if (segs.length > 0) {
      const lastSeg = segs[segs.length - 1];
      const lastSegEnd = new Date(lastSeg.started_at).getTime() + lastSeg.duration_sec * 1000;
      wallSpanSec = (lastSegEnd - startedAt.getTime()) / 1000;
    }
    // Also ensure we cover at least the last telemetry point.
    // Add one interval's worth of padding (10 s) so the last bar has visible width.
    if (points.length > 0) {
      wallSpanSec = Math.max(wallSpanSec, points[points.length - 1].offset_sec + 10);
    }

    // Store telemetry on card for click/hover/playhead handlers.
    // durationSec is this card's own span (used as fallback); the shared window
    // is stored in _sharedWindowMs and used for drawing.
    card._telemetry = { points, startedAt, durationSec: wallSpanSec };

    // Recompute shared window (this also redraws all timelines).
    recomputeSharedTimeWindow();

    // Wire click handler (only once — guard with a flag).
    if (!canvas._handlersWired) {
      canvas._handlersWired = true;

      // Click → seek
      canvas.onclick = e => {
        const win = _sharedWindowMs;
        if (!win) return;
        const rect = canvas.getBoundingClientRect();
        const frac = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
        const windowSec = (win.endMs - win.startMs) / 1000;
        const targetMs  = win.startMs + frac * (win.endMs - win.startMs);
        const targetDate = new Date(targetMs);

        // Exclude live segments from seek — they have started_at = now and
        // would cause findSegmentAt to always fall through to the live segment.
        const segments = (card._segments || []).filter(s => !s._live);
        if (!segments.length) return;
        const result = findSegmentAt(segments, targetDate);
        if (!result) return;

        // Ensure the session player is visible before seeking.
        const playerDiv = card.querySelector('.session-player');
        const header    = card.querySelector('.session-header');
        if (playerDiv && playerDiv.classList.contains('hidden')) {
          playerDiv.classList.remove('hidden');
          if (header) {
            const toggle = header.querySelector('.sess-toggle');
            if (toggle) toggle.textContent = '▼';
          }
        }
        sessLoadSegment(card, result.seg, result.offsetSecs);
      };

      // ── Hover tooltip ────────────────────────────────────────────────────
      let tip = card._snrTip;
      if (!tip) {
        tip = document.createElement('div');
        tip.className = 'snr-tip';
        document.body.appendChild(tip);
        card._snrTip = tip;
      }

      canvas.addEventListener('mousemove', e => {
        const win = _sharedWindowMs;
        const tel = card._telemetry;
        if (!win || !tel) return;
        const rect = canvas.getBoundingClientRect();
        const frac = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
        const targetMs   = win.startMs + frac * (win.endMs - win.startMs);
        const targetDate = new Date(targetMs);
        // offset_sec in telemetry is relative to this card's own startedAt
        const targetSec  = (targetMs - tel.startedAt.getTime()) / 1000;

        // Find nearest telemetry point for SNR value.
        const pts = tel.points;
        let nearest = pts[0];
        let bestDist = Math.abs(pts[0].offset_sec - targetSec);
        for (let i = 1; i < pts.length; i++) {
          const d = Math.abs(pts[i].offset_sec - targetSec);
          if (d < bestDist) { bestDist = d; nearest = pts[i]; }
        }

        const hh = String(targetDate.getHours()).padStart(2, '0');
        const mm = String(targetDate.getMinutes()).padStart(2, '0');
        const ss = String(targetDate.getSeconds()).padStart(2, '0');
        const snrVal = nearest && nearest.snr ? nearest.snr.avg_db.toFixed(1) + ' dB' : '—';
        tip.textContent = `${hh}:${mm}:${ss}  SNR ${snrVal}`;
        tip.style.display = 'block';
        const tipW = 160;
        let left = e.clientX - tipW / 2;
        left = Math.max(4, Math.min(left, window.innerWidth - tipW - 4));
        tip.style.left = left + 'px';
        tip.style.top  = (e.clientY - 36) + 'px';
      });

      canvas.addEventListener('mouseleave', () => {
        if (card._snrTip) card._snrTip.style.display = 'none';
      });
    }
  }).catch(() => {
    canvas.classList.add('hidden');
    if (empty) empty.classList.remove('hidden');
  });
}

// drawSnrTimeline — draw the SNR bar chart on canvas.
//
// Parameters:
//   canvas      — the <canvas> element
//   points      — telemetry points (offset_sec relative to startedAt)
//   startedAt   — Date: wall-clock start of THIS card's recording
//   windowStartMs — shared window start (ms since epoch); bars outside this are clipped
//   windowEndMs   — shared window end   (ms since epoch)
//
// When windowStartMs/windowEndMs are omitted the function falls back to the
// card's own span (backward-compatible single-card behaviour).
function drawSnrTimeline(canvas, points, startedAt, windowStartMs, windowEndMs) {
  const AXIS_H = 14; // pixels reserved for time axis at bottom
  // Size canvas to its CSS width — only reassign when the size actually changes.
  // Assigning canvas.width/height (even to the same value) clears the canvas and
  // resets the 2D context state, which would corrupt the coordinate space when
  // called from the rAF playhead loop.
  const W = canvas.offsetWidth || canvas.parentElement.offsetWidth || 600;
  const H = canvas._fixedHeight || canvas.height || 72;
  if (canvas.width !== W || canvas.height !== H) {
    canvas.width  = W;
    canvas.height = H;
  }
  canvas._fixedHeight = H; // remember the intended height across redraws

  const ctx = canvas.getContext('2d');
  ctx.clearRect(0, 0, W, H);

  // Background for SNR area
  const snrH = H - AXIS_H;
  ctx.fillStyle = 'rgba(0,0,0,0.25)';
  ctx.fillRect(0, 0, W, snrH);

  // Background for time axis
  ctx.fillStyle = 'rgba(0,0,0,0.45)';
  ctx.fillRect(0, snrH, W, AXIS_H);

  // Resolve window bounds.
  const cardStartMs = startedAt ? startedAt.getTime() : 0;
  const winStartMs  = (windowStartMs != null) ? windowStartMs : cardStartMs;
  const winEndMs    = (windowEndMs   != null) ? windowEndMs   : (cardStartMs + (points.length > 0 ? (points[points.length-1].offset_sec + 10) * 1000 : 0));
  const windowSec   = Math.max(1, (winEndMs - winStartMs) / 1000);

  if (!points.length) return;

  // Compute SNR range across all points that fall within the window.
  const visiblePoints = points.filter(p => {
    const ptMs = cardStartMs + p.offset_sec * 1000;
    return ptMs >= winStartMs && ptMs <= winEndMs;
  });

  if (!visiblePoints.length) return;

  const snrValues = visiblePoints.map(p => p.snr.avg_db);
  const minSNR = Math.min(...snrValues);
  const maxSNR = Math.max(...snrValues);
  const rangeSNR = maxSNR - minSNR || 1;

  const pad = 3;

  // Draw SNR bars — each bar's x position is computed from the shared window.
  // Bar width extends to the next point (or 10 s for the last bar).
  points.forEach((p, i) => {
    const ptMs  = cardStartMs + p.offset_sec * 1000;
    // Skip points entirely outside the window.
    if (ptMs > winEndMs) return;

    const x = ((ptMs - winStartMs) / (winEndMs - winStartMs)) * W;

    let barEndMs;
    if (i < points.length - 1) {
      const nextPtMs = cardStartMs + points[i + 1].offset_sec * 1000;
      const gap = nextPtMs - ptMs;
      // If the gap to the next point is large (> 30 s) this is a recording gap.
      // Don't extend the bar across the gap — cap it at 10 s (one telemetry interval).
      barEndMs = gap <= 30000 ? nextPtMs : ptMs + 10000;
    } else {
      barEndMs = ptMs + 10000; // 10 s padding for last bar
    }
    const xEnd = ((barEndMs - winStartMs) / (winEndMs - winStartMs)) * W;
    const barW = Math.max(1, Math.min(xEnd, W) - Math.max(x, 0));

    if (x > W || xEnd < 0) return; // fully outside canvas

    const norm = (p.snr.avg_db - minSNR) / rangeSNR;
    const barH = pad + norm * (snrH - pad * 2);
    const y    = snrH - barH;
    const hue  = norm * 120; // 0=red, 120=green
    ctx.fillStyle = `hsl(${hue},80%,50%)`;
    ctx.fillRect(Math.max(x, 0), y, barW, barH);
  });

  // SNR range labels (top-left corner of SNR area)
  ctx.fillStyle = 'rgba(255,255,255,0.5)';
  ctx.font = '9px sans-serif';
  ctx.textBaseline = 'top';
  ctx.fillText(`${maxSNR.toFixed(0)} dB`, 3, 2);
  ctx.textBaseline = 'bottom';
  ctx.fillText(`${minSNR.toFixed(0)} dB`, 3, snrH - 2);

  // ── Time axis ──────────────────────────────────────────────────────────────
  // Choose a tick interval that gives ~5–10 ticks across the shared window.
  const candidates = [60, 300, 600, 900, 1800, 3600, 7200, 21600, 43200, 86400];
  const targetTicks = Math.max(3, Math.floor(W / 80));
  let tickSec = candidates[candidates.length - 1];
  for (const c of candidates) {
    if (windowSec / c <= targetTicks) { tickSec = c; break; }
  }

  ctx.fillStyle = 'rgba(255,255,255,0.6)';
  ctx.font = '9px sans-serif';
  ctx.textBaseline = 'middle';
  const axisY = snrH + AXIS_H / 2;

  // First tick: round up to next tickSec boundary from winStartMs
  const firstTickMs = Math.ceil(winStartMs / 1000 / tickSec) * tickSec * 1000;

  for (let tickMs = firstTickMs; tickMs <= winEndMs; tickMs += tickSec * 1000) {
    const x = ((tickMs - winStartMs) / (winEndMs - winStartMs)) * W;
    if (x < 0 || x > W) continue;
    // Tick mark
    ctx.fillStyle = 'rgba(255,255,255,0.3)';
    ctx.fillRect(x, snrH, 1, AXIS_H);
    // Label
    const labelDate = new Date(tickMs);
    const hh = String(labelDate.getHours()).padStart(2, '0');
    const mm = String(labelDate.getMinutes()).padStart(2, '0');
    const label = `${hh}:${mm}`;
    ctx.fillStyle = 'rgba(255,255,255,0.6)';
    ctx.font = '9px sans-serif';
    ctx.textAlign = 'center';
    if (x + 18 < W) ctx.fillText(label, x + 1, axisY);
  }
  ctx.textAlign = 'left'; // reset
}

// Convert a Date to a value suitable for <input type="datetime-local">
function toDatetimeLocal(d) {
  if (!d || isNaN(d)) return '';
  const pad = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

// Find which segment contains the given Date, and how many seconds into it.
// Segments must be sorted by started_at ascending.
//
// Strategy (CCTV-style, 1-second precision):
//   1. Each segment "owns" the time from its started_at up to the next segment's
//      started_at (or started_at + duration_sec if it's the last one).
//      This means gaps between segments are attributed to the segment that
//      precedes the gap — the user clicks in the gap and we seek to the end
//      of the preceding segment, which is the closest available audio.
//   2. Before the first segment → seek to start of first segment.
//   3. After the last segment's audio ends → seek to end of last segment.
//
// Returns {seg, offsetSecs} or null.
function findSegmentAt(segments, targetDate) {
  if (!segments.length) return null;
  const t = targetDate.getTime();

  // Before the first segment starts.
  const firstStart = new Date(segments[0].started_at).getTime();
  if (t <= firstStart) {
    return { seg: segments[0], offsetSecs: 0 };
  }

  // Walk segments. Each segment owns [segStart, nextSegStart).
  // For the last segment it owns [segStart, segStart + duration_sec).
  for (let i = 0; i < segments.length; i++) {
    const seg      = segments[i];
    const segStart = new Date(seg.started_at).getTime();
    const segAudioEnd = segStart + seg.duration_sec * 1000;

    // Determine the end of this segment's "ownership window".
    let windowEnd;
    if (i < segments.length - 1) {
      windowEnd = new Date(segments[i + 1].started_at).getTime();
    } else {
      windowEnd = segAudioEnd;
    }

    if (t >= segStart && t < windowEnd) {
      // Clamp offset to the actual audio duration (can't seek past end of file).
      const rawOffset = (t - segStart) / 1000;
      const offsetSecs = Math.min(rawOffset, Math.max(0, seg.duration_sec - 0.1));
      return { seg, offsetSecs };
    }
  }

  // After everything — seek to end of last segment.
  const last = segments[segments.length - 1];
  return { seg: last, offsetSecs: Math.max(0, last.duration_sec - 0.1) };
}

// Draw (or clear) the playhead line on the SNR timeline canvas.
//
// positionSec — seconds from the SHARED window start (winStartMs).
//               Pass null to just redraw the base chart without a playhead.
// windowSec   — total duration of the shared window in seconds (optional;
//               derived from _sharedWindowMs when omitted).
function drawPlayhead(card, positionSec, windowSec) {
  const canvas = card.querySelector('.snr-timeline');
  const tel    = card._telemetry;
  if (!canvas || !tel) return;

  // Resolve the shared window.
  const win = _sharedWindowMs;
  const winStartMs = win ? win.startMs : tel.startedAt.getTime();
  const winEndMs   = win ? win.endMs   : (tel.startedAt.getTime() + tel.durationSec * 1000);
  const winSec     = windowSec != null ? windowSec : Math.max(1, (winEndMs - winStartMs) / 1000);

  if (winSec <= 0) return;

  // Redraw the base chart first, then overlay the playhead.
  // drawSnrTimeline only resizes the canvas when dimensions actually change,
  // so the coordinate space stays stable across rAF ticks.
  drawSnrTimeline(canvas, tel.points, tel.startedAt, winStartMs, winEndMs);

  if (positionSec == null) return;

  const frac = Math.max(0, Math.min(1, positionSec / winSec));
  const x    = frac * canvas.width;
  const ctx  = canvas.getContext('2d');
  const snrH = canvas.height - 14; // AXIS_H = 14

  ctx.save();
  ctx.strokeStyle = 'rgba(255,255,255,0.9)';
  ctx.lineWidth   = 2;
  ctx.shadowColor = 'rgba(0,0,0,0.7)';
  ctx.shadowBlur  = 3;
  ctx.beginPath();
  ctx.moveTo(x, 0);
  ctx.lineTo(x, snrH);
  ctx.stroke();
  ctx.restore();
}

// Start a rAF loop that keeps the playhead in sync with audio.currentTime.
function startPlayheadLoop(card) {
  stopPlayheadLoop(card);
  const audio = card.querySelector('.sess-audio');
  if (!audio) return;

  function tick() {
    if (!card._telemetry) return;
    const seg = card._segments && card._segments[card._currentSegIdx ?? 0];
    if (!seg) return;

    // positionSec = offset of the current playback position from the SHARED window start.
    const win = _sharedWindowMs;
    const winStartMs = win ? win.startMs : card._telemetry.startedAt.getTime();
    const winEndMs   = win ? win.endMs   : (card._telemetry.startedAt.getTime() + card._telemetry.durationSec * 1000);
    const winSec     = Math.max(1, (winEndMs - winStartMs) / 1000);

    const segWallMs      = new Date(seg.started_at).getTime();
    const segOffsetFromWin = (segWallMs - winStartMs) / 1000;
    const positionSec    = segOffsetFromWin + (audio.currentTime || 0);
    drawPlayhead(card, positionSec, winSec);

    // Update the "now playing" time label in real time as audio plays.
    const nowLabel = card.querySelector('.sess-now-playing');
    if (nowLabel) {
      const currentWallTime = new Date(segWallMs + (audio.currentTime || 0) * 1000);
      const segMode  = (seg.audio_mode || '').toUpperCase();
      const segLabel = seg.label ? `${seg.label} · ` : '';
      const segFreq  = seg.freq_hz ? `${fmtFreq(seg.freq_hz)} ${segMode} · ` : '';
      nowLabel.textContent = `${segLabel}${segFreq}Seg ${seg.segment_index + 1} — ${fmtDate(currentWallTime)}`;
    }

    if (!audio.paused && !audio.ended) {
      card._playheadRAF = requestAnimationFrame(tick);
    } else {
      card._playheadRAF = null;
    }
  }
  card._playheadRAF = requestAnimationFrame(tick);
}

function stopPlayheadLoop(card) {
  if (card._playheadRAF) {
    cancelAnimationFrame(card._playheadRAF);
    card._playheadRAF = null;
  }
}

// Load a segment into the session player and seek to offsetSecs.
function sessLoadSegment(card, seg, offsetSecs) {
  const audio    = card.querySelector('.sess-audio');
  const nowLabel = card.querySelector('.sess-now-playing');
  const prevBtn  = card.querySelector('.sess-prev-btn');
  const nextBtn  = card.querySelector('.sess-next-btn');
  const segments = card._segments || [];
  const idx      = segments.findIndex(s => s.id === seg.id);

  // Live (in-progress) segments are served via the active-stream endpoint;
  // completed segments are served from the recordings file store.
  const url = seg._liveStreamUrl || `${BASE}/recordings/${encodeURIComponent(seg.filename)}`;

  // Store current index on the card for step buttons.
  card._currentSegIdx = idx;
  prevBtn.disabled = idx <= 0;
  nextBtn.disabled = idx >= segments.length - 1;

  // Update the Download Segment button group (WAV + MP3 dropdown).
  const dlSegWrap = card.querySelector('.sess-dl-seg-wrap');
  if (dlSegWrap) {
    if (!seg._live && seg.filename) {
      const segUrl  = `${BASE}/recordings/${encodeURIComponent(seg.filename)}`;
      const segName = seg.filename.replace(/^.*[\\/]/, ''); // basename only
      const wavLink = dlSegWrap.querySelector('.sess-dl-seg-wav');
      const mp3Btn  = dlSegWrap.querySelector('.sess-dl-seg-mp3');
      if (wavLink) { wavLink.href = segUrl; wavLink.download = segName; }
      if (mp3Btn)  { mp3Btn.dataset.segId = seg.id; mp3Btn.dataset.wavFilename = seg.filename; }
      dlSegWrap.classList.remove('hidden');
    } else {
      dlSegWrap.classList.add('hidden');
    }
  }

  // Show segment info + the seek offset so user can confirm the right time was selected.
  const seekTime = new Date(new Date(seg.started_at).getTime() + offsetSecs * 1000);
  const segMode  = (seg.audio_mode || '').toUpperCase();
  const segLabel = seg.label ? `${seg.label} · ` : '';
  const segFreq  = seg.freq_hz ? `${fmtFreq(seg.freq_hz)} ${segMode} · ` : '';
  nowLabel.textContent = `${segLabel}${segFreq}Seg ${seg.segment_index + 1} — ${fmtDate(seekTime)}`;

  // Highlight the matching segment row.
  card.querySelectorAll('.seg-row').forEach(r => r.classList.remove('seg-active'));
  const activeRow = card.querySelector(`.seg-row[data-id="${seg.id}"]`);
  if (activeRow) {
    activeRow.classList.add('seg-active');
    activeRow.scrollIntoView({ block: 'nearest' });
  }

  // When this segment ends, auto-advance to the next.
  // Read card._segments and card._currentSegIdx at fire-time so we always
  // use the latest segment list (may have grown since this call was made).
  audio.onended = () => {
    stopPlayheadLoop(card);
    const currentSegs = card._segments || [];
    const currentIdx  = card._currentSegIdx ?? 0;
    const next = currentSegs[currentIdx + 1];
    if (next) sessLoadSegment(card, next, 0);
  };

  // Live segments use a streaming endpoint that returns a snapshot of the
  // in-progress WAV at request time — always force a fresh fetch so the
  // browser doesn't replay a cached (shorter) response.
  // For completed segments, if the same file is already loaded we can seek
  // directly without reloading.
  let currentPath = '';
  try { if (audio.src) currentPath = new URL(audio.src).pathname; } catch (_) {}
  const targetPath = new URL(url, location.href).pathname;
  const alreadyLoaded = !seg._live && currentPath === targetPath && audio.readyState >= 1;

  if (offsetSecs <= 0) {
    // No seek needed — just set src (if changed) and play from the start.
    if (!alreadyLoaded) {
      audio.src = url;
    } else {
      audio.currentTime = 0;
    }
    audio.play().then(() => startPlayheadLoop(card)).catch(() => {});
  } else if (alreadyLoaded) {
    // Same completed file already loaded with metadata — seek directly.
    audio.currentTime = Math.min(offsetSecs, isFinite(audio.duration) ? audio.duration - 0.1 : offsetSecs);
    audio.play().then(() => startPlayheadLoop(card)).catch(() => {});
  } else {
    // Different file (or live segment) — set src and wait for metadata before seeking.
    audio.src = url;
    audio.addEventListener('loadedmetadata', () => {
      if (offsetSecs > 0) {
        audio.currentTime = Math.min(offsetSecs, isFinite(audio.duration) ? audio.duration - 0.1 : offsetSecs);
      }
      audio.play().then(() => startPlayheadLoop(card)).catch(() => {});
    }, { once: true });
    // Trigger load so loadedmetadata fires (preload="none" won't load otherwise).
    audio.load();
  }
}

// Called by the "▶ Go" button.
function seekSessionTo(btn) {
  const card     = btn.closest('.session-card');
  const input    = card.querySelector('.sess-seek-input');
  // Exclude live segments — they have started_at = now and break findSegmentAt.
  const segments = (card._segments || []).filter(s => !s._live);
  if (!segments.length) return;

  const target = new Date(input.value);
  if (isNaN(target)) { alert('Invalid date/time'); return; }

  const result = findSegmentAt(segments, target);
  if (!result) { alert('No segment found for that time'); return; }
  sessLoadSegment(card, result.seg, result.offsetSecs);
}

// Called by Prev / Next buttons.
function sessStepSegment(btn, delta) {
  const card     = btn.closest('.session-card');
  const segments = card._segments || [];
  const idx      = (card._currentSegIdx || 0) + delta;
  if (idx < 0 || idx >= segments.length) return;
  sessLoadSegment(card, segments[idx], 0);
}

function toggleSession(header) {
  const segs = header.closest('.session-card').querySelector('.session-segments');
  const arrow = header.querySelector('.sess-toggle');
  const open = !segs.classList.contains('hidden');
  segs.classList.toggle('hidden', open);
  arrow.textContent = open ? '▶' : '▼';
}

function renderPagination() {
  const prevBtn = document.getElementById('prev-btn');
  const nextBtn = document.getElementById('next-btn');
  const info    = document.getElementById('page-info');
  const page    = Math.floor(state.offset / PAGE_SIZE) + 1;
  const pages   = Math.max(1, Math.ceil(state.total / PAGE_SIZE));
  info.textContent = `Page ${page} of ${pages} (${state.total} total)`;
  prevBtn.disabled = state.offset === 0;
  nextBtn.disabled = state.offset + PAGE_SIZE >= state.total;
}

document.getElementById('prev-btn').onclick = () => {
  state.offset = Math.max(0, state.offset - PAGE_SIZE);
  loadRecordings();
};
document.getElementById('next-btn').onclick = () => {
  state.offset += PAGE_SIZE;
  loadRecordings();
};
document.getElementById('refresh-btn').onclick = () => {
  state.offset = 0;
  loadRecordings();
};
function clearRecordingsList() {
  const list = document.getElementById('recordings-list');
  // Stop any audio and cancel playhead loops before wiping the DOM.
  Array.from(list.querySelectorAll('.session-card')).forEach(card => {
    card.querySelectorAll('audio').forEach(a => { try { a.pause(); a.src = ''; } catch (_) {} });
    stopPlayheadLoop(card);
    if (card._snrTip) { card._snrTip.style.display = 'none'; }
  });
  list.innerHTML = '';
  _sharedWindowMs = null;
}

document.getElementById('label-filter').onchange = e => {
  state.channelId = e.target.value; // UUID (or '' for all channels)
  state.offset = 0;
  // Refresh the date dropdown first (scoped to the selected channel), then
  // load recordings using the resolved state.date so the two filters are
  // always consistent.
  clearRecordingsList();
  loadDates(() => loadRecordings());
};
document.getElementById('date-filter').onchange = e => {
  state.date = e.target.value;
  state.offset = 0;
  // Clear the list immediately so stale cards (including live-recording cards
  // from today) don't persist when switching to a different date.
  clearRecordingsList();
  loadRecordings();
};

function deleteSegment(id) {
  if (!confirm('Delete this segment?')) return;
  fetch(BASE + '/api/recordings/' + id, { method: 'DELETE' })
    .then(r => {
      if (!r.ok) return r.text().then(t => Promise.reject(t));
      // Reload sessions to reflect the change
      loadRecordings();
    })
    .catch(e => alert('Delete failed: ' + e));
}

function deleteSession(cardKey) {
  if (!confirm('Delete ALL recordings for this channel?')) return;
  fetch(BASE + '/api/sessions/' + cardKey, { method: 'DELETE' })
    .then(r => {
      if (!r.ok) return r.text().then(t => Promise.reject(t));
      // Find card by channel_id first, then session_id fallback.
      const card = document.querySelector(`.session-card[data-channel-id="${cardKey}"]`)
        || document.querySelector(`.session-card[data-session-id="${cardKey}"]`);
      if (card) { card.remove(); recomputeSharedTimeWindow(); }
      state.total = Math.max(0, state.total - 1);
      renderPagination();
    })
    .catch(e => alert('Delete failed: ' + e));
}

// ── SSE live feed ─────────────────────────────────────────────────────────────

function connectSSE() {
  const es = new EventSource(BASE + '/api/live');

  es.addEventListener('recording_saved', e => {
    loadChannels();
    loadDates(); // refresh date picker in case a new day has started
    // Only reload recordings when viewing today or all-dates — past dates are
    // immutable so there is nothing new to show.
    if (isViewingLiveDate() && state.offset === 0) loadRecordings();
    // Also refresh the SNR timeline for the affected channel card immediately.
    if (isViewingLiveDate()) {
      try {
        const ev = JSON.parse(e.data);
        const channelId = ev.data && ev.data.channel_id;
        if (channelId) {
          const card = document.querySelector(`.session-card[data-channel-id="${channelId}"]`);
          if (card) loadSnrTimeline(card, channelId);
        }
      } catch (_) {}
    }
  });

  es.addEventListener('recording_started', e => {
    loadChannels();
    // Play ding if bell is enabled for this channel.
    try {
      const ev = JSON.parse(e.data);
      const label = ev.data && ev.data.label;
      if (label && isBellEnabled(label)) playDing();
    } catch (_) {}
  });

  es.addEventListener('recording_deleted', () => {
    // Reload sessions to reflect deletion — only matters for live/all-dates view.
    if (isViewingLiveDate()) loadRecordings();
  });

  es.addEventListener('session_deleted', e => {
    const ev = JSON.parse(e.data);
    const sid = ev.data && ev.data.session_id;
    if (sid) {
      // Find card by channel_id (preferred) or session_id fallback.
      const card = document.querySelector(`.session-card[data-channel-id="${sid}"]`)
        || document.querySelector(`.session-card[data-session-id="${sid}"]`);
      if (card) { card.remove(); recomputeSharedTimeWindow(); }
      state.total = Math.max(0, state.total - 1);
      renderPagination();
    }
  });

  es.addEventListener('channel_added', () => {
    // Full reload so filter options and cards are consistent
    loadChannels();
  });

  es.addEventListener('channel_removed', e => {
    const ev = JSON.parse(e.data);
    const label = ev.data && ev.data.label;
    const channelId = ev.data && ev.data.channel_id;
    if (label) {
      // Remove card immediately
      const card = document.querySelector(`.channel-card[data-label="${label}"]`);
      if (card) card.remove();
      // Remove filter option — options are keyed by UUID (channel_id) when available,
      // falling back to label for old records without a UUID.
      const filter = document.getElementById('label-filter');
      Array.from(filter.options).forEach(opt => {
        if ((channelId && opt.value === channelId) || (!channelId && opt.value === label)) {
          opt.remove();
        }
      });
      // If currently filtered to this channel, reset to "All"
      const removedId = channelId || label;
      if (state.channelId === removedId) {
        state.channelId = '';
        filter.value = '';
        state.offset = 0;
        loadRecordings();
      }
    }
  });

  es.addEventListener('channel_renamed', () => {
    // Full reload so filter options, card labels, and recordings are consistent
    loadChannels();
    if (isViewingLiveDate()) loadRecordings();
  });

  es.onerror = () => {
    setTimeout(connectSSE, 5000);
    es.close();
  };
}

// ── Real-time SNR via SSE /api/snr/all ───────────────────────────────────────
// One SSE connection delivers [{label, snr}, ...] every 200 ms for all channels.
// Updates the .ch-snr text and draws a rolling 10-second SNR chart on .ch-snr-chart.

const SNR_HISTORY_LEN = 100;  // 100 samples × 100 ms = 10 s
const SNR_MIN_DB      = 30;   // red
const SNR_MAX_DB      = 60;   // green

// Ring buffers: label → Float32Array of length SNR_HISTORY_LEN
const snrHistory = {};

function snrToHue(db) {
  // Clamp to [SNR_MIN_DB, SNR_MAX_DB] then map to hue 0 (red) → 120 (green)
  const norm = Math.max(0, Math.min(1, (db - SNR_MIN_DB) / (SNR_MAX_DB - SNR_MIN_DB)));
  return norm * 120;
}

function drawChannelSNRChart(canvas, buf, count) {
  const W = canvas.offsetWidth || canvas.parentElement?.offsetWidth || 200;
  const H = canvas.height || 32;
  if (canvas.width !== W) canvas.width = W;

  const ctx = canvas.getContext('2d');
  ctx.clearRect(0, 0, W, H);

  // Dark background
  ctx.fillStyle = 'rgba(0,0,0,0.3)';
  ctx.fillRect(0, 0, W, H);

  if (count === 0) return;

  // Read ring buffer in chronological order (oldest → newest = left → right).
  // buf is a {data: Float32Array, head, count} object.
  const n = count;
  const startIdx = (buf.head - n + SNR_HISTORY_LEN) % SNR_HISTORY_LEN;
  const xStep = W / (SNR_HISTORY_LEN - 1 || 1);

  // Build point array in order
  const pts = [];
  for (let i = 0; i < n; i++) {
    const idx = (startIdx + i) % SNR_HISTORY_LEN;
    const db = buf.data[idx];
    const norm = Math.max(0, Math.min(1, (db - SNR_MIN_DB) / (SNR_MAX_DB - SNR_MIN_DB)));
    // x: spread across full width based on position in history
    const x = (i / (SNR_HISTORY_LEN - 1 || 1)) * W;
    const y = H - norm * (H - 2) - 1;
    pts.push({ x, y, norm });
  }

  if (pts.length < 2) {
    // Single point — draw a dot
    const p = pts[0];
    ctx.fillStyle = `hsl(${p.norm * 120},85%,55%)`;
    ctx.fillRect(p.x - 2, p.y - 2, 4, 4);
    return;
  }

  // Draw filled area with gradient colour along the line.
  // We draw the filled area first (semi-transparent), then the coloured line on top.

  // Filled area (use average colour for simplicity)
  const avgNorm = pts.reduce((s, p) => s + p.norm, 0) / pts.length;
  ctx.beginPath();
  ctx.moveTo(pts[0].x, H);
  ctx.lineTo(pts[0].x, pts[0].y);
  for (let i = 1; i < pts.length; i++) {
    ctx.lineTo(pts[i].x, pts[i].y);
  }
  ctx.lineTo(pts[pts.length - 1].x, H);
  ctx.closePath();
  ctx.fillStyle = `hsla(${avgNorm * 120},80%,45%,0.25)`;
  ctx.fill();

  // Coloured line — draw segment by segment so each segment gets its own colour.
  ctx.lineWidth = 1.5;
  ctx.lineJoin = 'round';
  for (let i = 1; i < pts.length; i++) {
    const norm = (pts[i - 1].norm + pts[i].norm) / 2;
    ctx.beginPath();
    ctx.strokeStyle = `hsl(${norm * 120},90%,55%)`;
    ctx.moveTo(pts[i - 1].x, pts[i - 1].y);
    ctx.lineTo(pts[i].x, pts[i].y);
    ctx.stroke();
  }
}

function connectSNRStream() {
  const es = new EventSource(BASE + '/api/snr/all');
  es.addEventListener('snr', e => {
    let entries;
    try { entries = JSON.parse(e.data); } catch { return; }
    if (!Array.isArray(entries)) return;
    entries.forEach(({ label, snr }) => {
      const card = document.querySelector(`.channel-card[data-label="${label}"]`);
      if (!card) return;

      // Update text
      const snrEl = card.querySelector('.ch-snr');
      if (snrEl) snrEl.textContent = 'SNR: ' + fmtSNR(snr);

      // Update rolling chart
      const canvas = card.querySelector('.ch-snr-chart');
      if (!canvas) return;

      if (!snrHistory[label]) {
        snrHistory[label] = { data: new Float32Array(SNR_HISTORY_LEN), head: 0, count: 0 };
      }
      const buf = snrHistory[label];
      const db = (snr && snr.avg_db != null) ? snr.avg_db : 0;
      buf.data[buf.head] = db;
      buf.head = (buf.head + 1) % SNR_HISTORY_LEN;
      if (buf.count < SNR_HISTORY_LEN) buf.count++;

      drawChannelSNRChart(canvas, buf, buf.count);
    });
  });
  es.onerror = () => {
    es.close();
    setTimeout(connectSNRStream, 3000);
  };
}

// Returns true when the current date filter is today — i.e. the view can
// contain live/changing data and is worth auto-refreshing.
function isViewingLiveDate() {
  return state.date === todayUTC();
}

// ── Init ──────────────────────────────────────────────────────────────────────

checkAuth().then(() => {
  loadChannels();
  loadDates();
  loadRecordings();
  loadRetention();
  loadQuota();
  connectSSE();
  connectSNRStream();
  // Full channel card refresh every 30 s (structural changes handled by SSE events)
  setInterval(loadChannels, 30000);
  // Auto-refresh recordings every 60 s — skip when viewing a past date because
  // historical data is immutable and there is nothing new to show.
  setInterval(() => { if (isViewingLiveDate()) loadRecordings(); }, 60000);
  // Refresh SNR timeline for live session cards every 60 s so the chart
  // stays current while a segment is still being recorded.
  setInterval(() => {
    if (!isViewingLiveDate()) return;
    document.querySelectorAll('.session-card.session-live').forEach(card => {
      const channelId = card.dataset.channelId;
      if (channelId) loadSnrTimeline(card, channelId);
    });
  }, 60000);
});
