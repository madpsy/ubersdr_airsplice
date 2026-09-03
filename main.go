// main.go — ubersdr_airsplice: multi-channel simultaneous audio recorder
//
// Channels are persisted to channels.json inside the output directory.
// Add/remove channels via the web UI; they survive restarts automatically.
//
// Usage:
//
//	ubersdr_airsplice -url ws://sdr.example.com/ws \
//	                  -listen :6095 \
//	                  -output /data
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// channelConfig — one entry in channels.json
// ---------------------------------------------------------------------------

// SNR SCALE — CHANGED WITH AUDIO PROTOCOL VERSION 4
// ------------------------------------------------
// The thresholds below are in dB of SNR as the server now reports it, and that
// scale moved when this addon migrated from protocol version 2 to version 4.
//
// Version 2 put radiod's raw noise DENSITY N0 (dBFS/Hz) in the packet header,
// while baseband power is a power integrated over the whole passband (dBFS).
// Subtracting one from the other therefore did not yield an SNR at all; it
// yielded S/N0 in dB·Hz, reading 10·log10(passband Hz) too high. From version 3
// the server sends the noise POWER inside the demodulator passband instead
// (ka9q_ubersdr/websocket.go, channelNoisePower in radiod_status.go), so the
// same subtraction is a true SNR.
//
// Every reading is therefore LOWER on version 4 by 10·log10(passband Hz):
//
//	mode        passband          correction
//	CW          500 Hz            27.0 dB
//	USB/LSB     2400 Hz           33.8 dB   (this addon's 2.7 kHz default)
//	USB/LSB     2950 Hz           34.7 dB   (radiod preset, when no bandwidth
//	                                         is configured)
//	AM/SAM      5000 Hz           37.0 dB
//	FM/NFM      8000 Hz           39.0 dB
//
// The old figure also drifted with the filter width; the new one does not.
//
// WHERE THE DEFAULTS COME FROM: MEASUREMENT, NOT ARITHMETIC
// ---------------------------------------------------------
// Rescaling the old 50/35 by that correction gives 16/1, and BOTH are wrong
// against the live receiver. They were checked on m9psy.tunnel.ubersdr.org on
// 2026-09-03, through this addon's own v4 decoder, at the 2.4 kHz SSB passband
// these defaults are for, sampling the gate-visible statistic (the mean of the
// last two packets, as peekLatest(2) computes it):
//
//	IDLE / no signal   6 captures, ~20,000 packets
//	  7.13/7.18/14.2/13.9 MHz and two 90 s captures on 7.12/7.15 MHz
//	  median  -0.8 .. +2.0     p90  0.3 .. 3.8
//	  p99      1.1 .. 7.1      max  9.3 .. 12.9
//
//	MODERATE SSB QSO   7.150 MHz LSB, station working
//	  median   7.95            p95 12.94     max 16.43
//
//	STRONG SIGNAL      independently measured at 12 kHz USB
//	  median  32.67            p95 35.19     max 42.40
//
// An empty channel reads ~0 dB, which is what a true passband SNR must do and
// is the strongest confirmation that the units derivation above is right.
//
// Two things follow, and both contradict the arithmetic:
//
//	START 16 was too HIGH. A normal SSB QSO peaks around 16 dB and sustains
//	8-13; only an exceptionally strong station clears 16. A voice recorder that
//	only catches strong stations is broken. 10 dB sits above every idle capture's
//	p99 (<=7.1) with idle duty above 10 dB measured at 0.0-0.1%, and still
//	catches the moderate QSO.
//
//	STOP 1 was far too LOW -- it is inside the idle spread, not below it. Idle
//	duty above 1 dB was measured at 12.7%, 31.2% and 75.0% on three channels, so
//	the tail countdown would be cancelled by ordinary noise and a recording might
//	never close. 5 dB is above idle p90 on every capture (<=3.8) and above p95 on
//	both long ones; the longest continuously-below-5 run on the active channel
//	was ~39 s, far more than the 5 s the tail needs.
//
// HYSTERESIS: 5 dB, DELIBERATELY NOT 15
// -------------------------------------
// The old 15 dB gap cannot be kept. At 2.4 kHz the measured distance between the
// top of the idle spread (p99 ~7) and a sustained moderate QSO (~8-16) is only
// about 10 dB, so a 15 dB gap would force one threshold outside its population:
// that is exactly how 16/1 ended up straddling the wrong sides. 5 dB is the
// widest gap that keeps start above idle and stop below normal speech, and it is
// wide enough to stop the gate chattering on a marginal signal.
//
// Someone who only wants strong stations should RAISE start; the measurements
// above say a strong signal sits near 30 dB, so there is plenty of room.
//
// ANYONE CARRYING A THRESHOLD OVER FROM A PRE-VERSION-4 BUILD MUST SUBTRACT THE
// CORRECTION FOR THEIR MODE — about 34 dB on SSB. A threshold left on the old
// scale is roughly 34 dB above anything the server will ever report now, so the
// gate never opens and the channel records nothing, silently.
const (
	// defaultSmartStartThreshDB is the default start_thresh_db: SNR must
	// exceed this for start_hold_sec before a segment opens. Was 50 on the
	// version 2 scale; set from the measured idle and QSO populations above,
	// not by rescaling that number.
	defaultSmartStartThreshDB = 10.0

	// defaultSmartStopThreshDB is the default stop_thresh_db: SNR falling
	// below this begins the tail countdown. Was 35 on the version 2 scale;
	// set above the measured idle spread so the tail can actually complete.
	defaultSmartStopThreshDB = 5.0
)

// SmartRecordConfig holds the VOX-style SNR-gated recording parameters for a
// channel.  When Enabled is true the recorder only writes audio to disk while
// the SNR is above StartThreshDB for at least StartHoldSec seconds, and stops
// writing when the SNR falls below StopThreshDB for at least StopHoldSec
// seconds.
//
// Both thresholds are on the version 4 SNR scale; see the block above before
// changing them or carrying a value over from an older build.
type SmartRecordConfig struct {
	Enabled       bool    `json:"enabled"`
	StartThreshDB float32 `json:"start_thresh_db"` // SNR must exceed this to start
	StartHoldSec  float32 `json:"start_hold_sec"`  // must stay above for this long
	StopThreshDB  float32 `json:"stop_thresh_db"`  // SNR must fall below this to stop
	StopHoldSec   float32 `json:"stop_hold_sec"`   // must stay below for this long
	MaxRecordMins float32 `json:"max_record_mins"` // max recording length in minutes; 0 = unlimited
}

type channelConfig struct {
	ID          string            `json:"id"` // stable UUID; generated once, never changes
	FreqHz      int               `json:"freq_hz"`
	Mode        string            `json:"mode"`
	Name        string            `json:"name,omitempty"`         // user-defined display name; defaults to "{freq}_{mode}"
	SmartRecord SmartRecordConfig `json:"smart_record,omitempty"` // VOX-style gated recording
	Schedule    ScheduleConfig    `json:"schedule,omitempty"`     // time-based scheduled recording
	BandwidthHz int               `json:"bandwidth_hz,omitempty"` // filter bandwidth in Hz; 0 = server default
	MaxMB       int64             `json:"max_mb,omitempty"`       // per-channel storage quota in MB; 0 = use overall limit
}

// ---------------------------------------------------------------------------
// channelManager — thread-safe registry of live recChannels
// ---------------------------------------------------------------------------

type channelManager struct {
	mu          sync.RWMutex
	wg          sync.WaitGroup // tracks running recChannel goroutines
	channels    []*recChannel
	ubersdrURL  string
	password    string
	segmentSecs int
	store       *recordingStore
	hub         *sseHub
	ctx         context.Context
	configPath  string // path to channels.json
	quota       *quotaConfig
}

func newChannelManager(ctx context.Context, ubersdrURL, password string, segmentSecs int, store *recordingStore, hub *sseHub, configPath string, qc *quotaConfig) *channelManager {
	return &channelManager{
		ubersdrURL:  ubersdrURL,
		password:    password,
		segmentSecs: segmentSecs,
		store:       store,
		hub:         hub,
		ctx:         ctx,
		configPath:  configPath,
		quota:       qc,
	}
}

// add starts a new channel and registers it. Returns error if label already exists.
// name is optional; if empty the label defaults to "{freq}_{mode}".
// channelID is the stable UUID for this channel; if empty a new one is generated.
func (m *channelManager) add(freqHz int, mode, name, channelID string, sr SmartRecordConfig, sched ScheduleConfig, bandwidthHz int) (*recChannel, error) {
	label := name
	if label == "" {
		label = fmt.Sprintf("%d_%s", freqHz, mode)
	}
	if channelID == "" {
		channelID = uuid.New().String()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.channels {
		if ch.label == label {
			return nil, fmt.Errorf("channel %s already exists", label)
		}
	}

	inst := newInstance(freqHz, 0, mode, m.ubersdrURL, m.password, name, bandwidthHz)
	ch := newRecChannel(inst, m.store, m.hub, m.segmentSecs, channelID, sr, sched)
	m.channels = append(m.channels, ch)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ch.run(m.ctx)
	}()
	log.Printf("[manager] added channel %s (id %s)", label, channelID[:8])
	return ch, nil
}

// setSmartRecord updates the smart-record config for a channel by label.
// Returns error if the channel is not found.
func (m *channelManager) setSmartRecord(label string, sr SmartRecordConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.channels {
		if ch.label == label {
			ch.setSmartRecord(sr)
			return nil
		}
	}
	return fmt.Errorf("channel %q not found", label)
}

// setSchedule updates the schedule config for a channel by label.
// Returns error if the channel is not found.
func (m *channelManager) setSchedule(label string, sched ScheduleConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.channels {
		if ch.label == label {
			ch.setSchedule(sched)
			return nil
		}
	}
	return fmt.Errorf("channel %q not found", label)
}

// setBandwidth updates the filter bandwidth for a channel by label.
// The new value takes effect on the next reconnect.
// Returns error if the channel is not found.
func (m *channelManager) setBandwidth(label string, bandwidthHz int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.channels {
		if ch.label == label {
			ch.inst.setBandwidth(bandwidthHz)
			return nil
		}
	}
	return fmt.Errorf("channel %q not found", label)
}

// remove stops and removes a channel by label. Returns error if not found.
func (m *channelManager) remove(label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, ch := range m.channels {
		if ch.label == label {
			ch.inst.stop()
			ch.closeSegment()
			m.channels = append(m.channels[:i], m.channels[i+1:]...)
			log.Printf("[manager] removed channel %s", label)
			return nil
		}
	}
	return fmt.Errorf("channel %s not found", label)
}

// rename changes the display label of a channel without changing its UUID.
// Returns the new label. Returns error if oldLabel not found or newName conflicts.
func (m *channelManager) rename(oldLabel, newName string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var target *recChannel
	for _, ch := range m.channels {
		if ch.label == oldLabel {
			target = ch
		} else if ch.label == newName {
			return "", fmt.Errorf("channel %q already exists", newName)
		}
	}
	if target == nil {
		return "", fmt.Errorf("channel %q not found", oldLabel)
	}

	target.label = newName
	target.inst.label = newName
	// Also update the label on any in-progress segment so the final sidecar
	// reflects the new name.
	target.mu.Lock()
	if target.current != nil {
		target.current.label = newName
	}
	target.mu.Unlock()

	log.Printf("[manager] renamed channel %q → %q (id %s)", oldLabel, newName, target.channelID[:8])
	return newName, nil
}

// list returns a snapshot of current channels (safe to iterate without lock).
func (m *channelManager) list() []*recChannel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*recChannel, len(m.channels))
	copy(out, m.channels)
	return out
}

// save writes the current channel list to channels.json atomically.
// Must NOT be called while m.mu is held.
func (m *channelManager) save() {
	if m.configPath == "" {
		return
	}
	m.mu.RLock()
	cfgs := make([]channelConfig, 0, len(m.channels))
	for _, ch := range m.channels {
		// Only store the name if it differs from the auto-generated label.
		autoLabel := fmt.Sprintf("%d_%s", ch.inst.freqHz, ch.inst.audioMode)
		name := ""
		if ch.label != autoLabel {
			name = ch.label
		}
		// Read per-channel quota from the quota config.
		var maxMB int64
		if m.quota != nil {
			maxMB = m.quota.getForLabel(ch.label) / 1024 / 1024
		}
		cfgs = append(cfgs, channelConfig{
			ID:          ch.channelID,
			FreqHz:      ch.inst.freqHz,
			Mode:        ch.inst.audioMode,
			Name:        name,
			SmartRecord: ch.getSmartRecord(),
			Schedule:    ch.getSchedule(),
			BandwidthHz: ch.inst.getBandwidth(),
			MaxMB:       maxMB,
		})
	}
	m.mu.RUnlock()

	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		log.Printf("[manager] save channels: marshal: %v", err)
		return
	}
	tmp := m.configPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("[manager] save channels: write tmp: %v", err)
		return
	}
	if err := os.Rename(tmp, m.configPath); err != nil {
		log.Printf("[manager] save channels: rename: %v", err)
		return
	}
	log.Printf("[manager] saved %d channel(s) to %s", len(cfgs), m.configPath)
}

// load reads channels.json and starts each channel. Errors are logged, not fatal.
func (m *channelManager) load() {
	if m.configPath == "" {
		return
	}
	data, err := os.ReadFile(m.configPath)
	if os.IsNotExist(err) {
		log.Printf("[manager] no channels.json found — starting with no channels")
		return
	}
	if err != nil {
		log.Printf("[manager] load channels: %v", err)
		return
	}
	var cfgs []channelConfig
	if err := json.Unmarshal(data, &cfgs); err != nil {
		log.Printf("[manager] load channels: parse: %v", err)
		return
	}
	for _, cfg := range cfgs {
		if _, err := m.add(cfg.FreqHz, cfg.Mode, cfg.Name, cfg.ID, cfg.SmartRecord, cfg.Schedule, cfg.BandwidthHz); err != nil {
			log.Printf("[manager] load: %v", err)
			continue
		}
		// Restore per-channel quota into the quota config.
		if cfg.MaxMB > 0 && m.quota != nil {
			label := cfg.Name
			if label == "" {
				label = fmt.Sprintf("%d_%s", cfg.FreqHz, cfg.Mode)
			}
			m.quota.setForLabel(label, cfg.MaxMB)
		}
	}
	log.Printf("[manager] loaded %d channel(s) from %s", len(cfgs), m.configPath)
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func envInt64Or(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func main() {
	var (
		ubersdrURL = flag.String("url", envOr("UBERSDR_URL", ""), "UberSDR WebSocket URL (e.g. ws://host/ws) (env: UBERSDR_URL)")
		password   = flag.String("password", envOr("UBERSDR_PASS", ""), "UberSDR password (optional) (env: UBERSDR_PASS)")
		listenAddr = flag.String("listen", ":"+envOr("WEB_PORT", "6095"), "HTTP listen address (env: WEB_PORT)")
		outputDir  = flag.String("output", envOr("OUTPUT_DIR", "./recordings"), "Directory to save recordings (env: OUTPUT_DIR)")
		uiPassword = flag.String("ui-password", envOr("UI_PASSWORD", ""),
			"Password required for write actions in the web UI (env: UI_PASSWORD; empty = write actions disabled)")

		segmentSecs = flag.Int("segment-secs", envIntOr("SEGMENT_SECS", 300),
			"Recording segment length in seconds; 0 = continuous (env: SEGMENT_SECS)")

		cleanupAllDays = flag.Int("cleanup-all-days", envIntOr("CLEANUP_ALL_DAYS", 30),
			"Delete ALL recordings older than N days; 0 = disabled (env: CLEANUP_ALL_DAYS)")

		maxStorageMB = flag.Int64("max-storage-mb", envInt64Or("MAX_STORAGE_MB", 20480),
			"Maximum total storage in MB across all channels; 0 = unlimited, default 20480 (20 GB) (env: MAX_STORAGE_MB)")
	)

	flag.Parse()

	if *ubersdrURL == "" {
		fmt.Fprintln(os.Stderr, "error: -url (or UBERSDR_URL env) is required")
		flag.Usage()
		os.Exit(1)
	}

	// channels.json lives alongside the recordings in the output directory.
	configPath := filepath.Join(*outputDir, "channels.json")

	log.Printf("[main] ubersdr_airsplice starting")
	log.Printf("[main] UberSDR URL:   %s", *ubersdrURL)
	log.Printf("[main] Output dir:    %s", *outputDir)
	log.Printf("[main] Listen addr:   %s", *listenAddr)
	log.Printf("[main] Segment secs:  %d", *segmentSecs)
	log.Printf("[main] Channels cfg:  %s", configPath)
	if *maxStorageMB > 0 {
		log.Printf("[main] Max storage:   %d MB", *maxStorageMB)
	}

	hub := newSSEHub()
	store := newRecordingStore(*outputDir, hub)

	// Load retention config from disk (falls back to CLI keepDays default).
	retentionCfgPath := filepath.Join(*outputDir, "retention.json")
	rc := newRetentionConfig()
	rc.load(retentionCfgPath)

	// Load quota config from disk (falls back to CLI maxStorageMB default).
	quotaCfgPath := filepath.Join(*outputDir, "quota.json")
	qc := newQuotaConfig()
	qc.load(quotaCfgPath)

	startCleanup(store, *outputDir, *cleanupAllDays, rc, qc, *maxStorageMB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := newChannelManager(ctx, *ubersdrURL, *password, *segmentSecs, store, hub, configPath, qc)

	// Load persisted channels from channels.json.
	mgr.load()

	// Start HTTP server in background.
	go func() {
		if err := startHTTPServer(*listenAddr, store, hub, mgr, *uiPassword, rc, retentionCfgPath, qc, quotaCfgPath); err != nil {
			log.Fatalf("[main] HTTP server: %v", err)
		}
	}()

	// Wait for SIGINT / SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("[main] shutting down…")
	cancel()
	// Wait for all recChannel goroutines to finish closing their segments
	// before the process exits, so WAV headers and JSON sidecars are written.
	mgr.wg.Wait()
	log.Printf("[main] done")
}

// parseChannelSpec parses "7880000:usb" → (7880000, "usb", nil).
// Kept for potential future use (e.g. migration helpers).
func parseChannelSpec(spec string) (int, string, error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("expected freq:mode, got %q", spec)
	}
	freq, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, "", fmt.Errorf("invalid frequency %q: %w", parts[0], err)
	}
	mode := strings.TrimSpace(parts[1])
	if mode == "" {
		return 0, "", fmt.Errorf("empty mode in %q", spec)
	}
	return freq, mode, nil
}
