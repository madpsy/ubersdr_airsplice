/* AirSplice — app.js */
'use strict';

const BASE = window.BASE_PATH || '';
const PAGE_SIZE = 50;

let state = {
  offset: 0,
  label: '',
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
  if (!snr || snr.count === 0) return '—';
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

// ── Channels ─────────────────────────────────────────────────────────────────

function loadChannels() {
  api('/api/channels').then(channels => {
    const grid = document.getElementById('channels-grid');
    const filter = document.getElementById('label-filter');

    // Rebuild filter options
    const existing = new Set(Array.from(filter.options).map(o => o.value));
    (channels || []).forEach(ch => {
      if (!existing.has(ch.label)) {
        const opt = document.createElement('option');
        opt.value = ch.label;
        opt.textContent = ch.label;
        filter.appendChild(opt);
      }
    });

    grid.innerHTML = '';
    (channels || []).forEach(ch => {
      const card = document.createElement('div');
      card.className = 'channel-card';
      const statusClass = ch.recording ? 'recording' : (ch.status || 'stopped');
      const statusLabel = ch.recording ? '● Recording' : (ch.status || 'stopped');
      card.innerHTML = `
        <div class="ch-label">${ch.label}</div>
        <div class="ch-freq">${fmtFreq(ch.freq_hz)}</div>
        <div class="ch-mode">${ch.audio_mode || ''}</div>
        <span class="ch-status ${statusClass}">${statusLabel}</span>
        <div class="ch-snr">SNR: ${fmtSNR(ch.snr)}</div>
        <div class="ch-actions">
          <button onclick="openAudioPreview('${ch.label}')">▶ Preview</button>
        </div>`;
      grid.appendChild(card);
    });
  }).catch(e => console.warn('channels:', e));
}

function openAudioPreview(label) {
  const url = `${BASE}/api/audio/preview?label=${encodeURIComponent(label)}`;
  window.open(url, '_blank');
}

// ── Recordings ────────────────────────────────────────────────────────────────

function loadRecordings() {
  const params = new URLSearchParams({
    limit: PAGE_SIZE,
    offset: state.offset,
  });
  if (state.label) params.set('label', state.label);

  api('/api/recordings?' + params).then(data => {
    state.total = data.count || 0;
    renderRecordings(data.recordings || []);
    renderPagination();
  }).catch(e => {
    document.getElementById('recordings-list').innerHTML =
      `<div class="empty">Error loading recordings: ${e}</div>`;
  });
}

function renderRecordings(recs) {
  const list = document.getElementById('recordings-list');
  if (!recs.length) {
    list.innerHTML = '<div class="empty">No recordings yet.</div>';
    return;
  }
  list.innerHTML = '';
  recs.forEach(r => {
    const row = document.createElement('div');
    row.className = 'rec-row';
    row.dataset.id = r.id;
    const dlUrl = `${BASE}/recordings/${encodeURIComponent(r.filename)}`;
    row.innerHTML = `
      <span class="rec-label">${r.label}</span>
      <span class="rec-freq">${fmtFreq(r.freq_hz)}</span>
      <span class="rec-time">${fmtDate(r.started_at)}</span>
      <span class="rec-dur">${fmtDuration(r.duration_sec)}</span>
      <span class="rec-snr">SNR: ${fmtSNR(r.snr)}</span>
      <div class="rec-actions">
        <a href="${dlUrl}" download="${r.filename}">⬇ Download</a>
        ${state.authed ? `<button onclick="deleteRecording('${r.id}')">🗑 Delete</button>` : ''}
      </div>`;
    list.appendChild(row);
  });
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
document.getElementById('label-filter').onchange = e => {
  state.label = e.target.value;
  state.offset = 0;
  loadRecordings();
};

function deleteRecording(id) {
  if (!confirm('Delete this recording?')) return;
  fetch(BASE + '/api/recordings/' + id, { method: 'DELETE' })
    .then(r => {
      if (!r.ok) return r.text().then(t => Promise.reject(t));
      const row = document.querySelector(`.rec-row[data-id="${id}"]`);
      if (row) row.remove();
      state.total = Math.max(0, state.total - 1);
      renderPagination();
    })
    .catch(e => alert('Delete failed: ' + e));
}

// ── SSE live feed ─────────────────────────────────────────────────────────────

function connectSSE() {
  const es = new EventSource(BASE + '/api/live');

  es.addEventListener('recording_saved', e => {
    const ev = JSON.parse(e.data);
    const rec = ev.data;
    if (state.offset === 0 && (!state.label || state.label === rec.label)) {
      prependRecording(rec);
    }
    // Refresh channel cards to update segment info
    loadChannels();
  });

  es.addEventListener('recording_started', () => {
    loadChannels();
  });

  es.addEventListener('recording_deleted', e => {
    const ev = JSON.parse(e.data);
    const id = ev.data && ev.data.id;
    if (id) {
      const row = document.querySelector(`.rec-row[data-id="${id}"]`);
      if (row) row.remove();
      state.total = Math.max(0, state.total - 1);
      renderPagination();
    }
  });

  es.onerror = () => {
    setTimeout(connectSSE, 5000);
    es.close();
  };
}

function prependRecording(r) {
  const list = document.getElementById('recordings-list');
  const empty = list.querySelector('.empty');
  if (empty) empty.remove();

  const row = document.createElement('div');
  row.className = 'rec-row';
  row.dataset.id = r.id;
  const dlUrl = `${BASE}/recordings/${encodeURIComponent(r.filename)}`;
  row.innerHTML = `
    <span class="rec-label">${r.label}</span>
    <span class="rec-freq">${fmtFreq(r.freq_hz)}</span>
    <span class="rec-time">${fmtDate(r.started_at)}</span>
    <span class="rec-dur">${fmtDuration(r.duration_sec)}</span>
    <span class="rec-snr">SNR: ${fmtSNR(r.snr)}</span>
    <div class="rec-actions">
      <a href="${dlUrl}" download="${r.filename}">⬇ Download</a>
      ${state.authed ? `<button onclick="deleteRecording('${r.id}')">🗑 Delete</button>` : ''}
    </div>`;
  list.insertBefore(row, list.firstChild);
  state.total++;
  renderPagination();
}

// ── Init ──────────────────────────────────────────────────────────────────────

checkAuth().then(() => {
  loadChannels();
  loadRecordings();
  connectSSE();
  // Refresh channel status every 10 s
  setInterval(loadChannels, 10000);
});
