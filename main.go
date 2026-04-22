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

// SmartRecordConfig holds the VOX-style SNR-gated recording parameters for a
// channel.  When Enabled is true the recorder only writes audio to disk while
// the SNR is above StartThreshDB for at least StartHoldSec seconds, and stops
// writing when the SNR falls below StopThreshDB for at least StopHoldSec
// seconds.
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
