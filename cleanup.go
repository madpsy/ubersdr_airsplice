// cleanup.go — background age-based cleanup of old recordings
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const cleanupInterval = 5 * time.Minute

// ---------------------------------------------------------------------------
// retentionConfig — live-updatable per-channel retention settings
// ---------------------------------------------------------------------------

// retentionConfig holds the current retention policy.
// Channels maps label → keep_hours (0 = use DefaultHours; if that is also 0 = keep forever).
// DefaultHours applies to any channel not explicitly listed.
type retentionConfig struct {
	mu           sync.RWMutex
	DefaultHours int            `json:"default_hours"`
	Channels     map[string]int `json:"channels"`
}

func newRetentionConfig() *retentionConfig {
	return &retentionConfig{Channels: make(map[string]int)}
}

// getForLabel returns the effective keep_hours for a given channel label.
// Returns 0 if no retention is configured (keep forever).
func (rc *retentionConfig) getForLabel(label string) int {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if h, ok := rc.Channels[label]; ok {
		return h
	}
	return rc.DefaultHours
}

// setDefault updates the default retention for channels not explicitly configured.
func (rc *retentionConfig) setDefault(hours int) {
	rc.mu.Lock()
	rc.DefaultHours = hours
	rc.mu.Unlock()
}

// setForLabel sets the retention for a specific channel label.
func (rc *retentionConfig) setForLabel(label string, hours int) {
	rc.mu.Lock()
	if rc.Channels == nil {
		rc.Channels = make(map[string]int)
	}
	if hours == 0 {
		delete(rc.Channels, label)
	} else {
		rc.Channels[label] = hours
	}
	rc.mu.Unlock()
}

// snapshot returns a copy of the config safe for JSON serialisation.
func (rc *retentionConfig) snapshot() (defaultHours int, channels map[string]int) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	ch := make(map[string]int, len(rc.Channels))
	for k, v := range rc.Channels {
		ch[k] = v
	}
	return rc.DefaultHours, ch
}

// load reads retention.json from configPath; silently ignores missing file.
func (rc *retentionConfig) load(configPath string) {
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		log.Printf("retention: load %s: %v", configPath, err)
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.Channels == nil {
		rc.Channels = make(map[string]int)
	}
	if err := json.Unmarshal(data, rc); err != nil {
		log.Printf("retention: parse %s: %v", configPath, err)
	}
}

// save writes retention.json atomically.
func (rc *retentionConfig) save(configPath string) {
	rc.mu.RLock()
	data, err := json.MarshalIndent(rc, "", "  ")
	rc.mu.RUnlock()
	if err != nil {
		return
	}
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("retention: save %s: %v", configPath, err)
		return
	}
	if err := os.Rename(tmp, configPath); err != nil {
		log.Printf("retention: rename %s: %v", configPath, err)
	}
}

// ---------------------------------------------------------------------------
// Background cleanup worker
// ---------------------------------------------------------------------------

// startAgeCleanup runs a ticker every 5 minutes and deletes recordings older
// than the configured retention period per channel.
// keepDays is the legacy CLI flag value used as the initial default when no
// retention.json exists and keepDays > 0.
func startAgeCleanup(store *recordingStore, outputDir string, keepDays int, rc *retentionConfig) {
	// Apply legacy CLI default if no retention.json was loaded.
	def, _ := rc.snapshot()
	if def == 0 && keepDays > 0 {
		rc.setDefault(keepDays * 24)
	}

	log.Printf("cleanup: age worker started (check every 5 min)")
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			runAgeCleanup(store, outputDir, rc)
		}
	}()
}

func runAgeCleanup(store *recordingStore, outputDir string, rc *retentionConfig) {
	store.mu.RLock()
	var candidates []*recordingRecord
	for _, r := range store.records {
		hours := rc.getForLabel(r.Label)
		if hours <= 0 {
			continue // keep forever
		}
		cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
		if r.SavedAt.Before(cutoff) {
			candidates = append(candidates, r)
		}
	}
	store.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}
	log.Printf("cleanup: age pass — %d recording(s) to delete", len(candidates))
	for _, rec := range candidates {
		deleteRecordFiles(store, outputDir, rec)
	}
}

func deleteRecordFiles(store *recordingStore, outputDir string, rec *recordingRecord) {
	id := rec.ID

	// Remove WAV file.
	if rec.Filename != "" {
		p := filepath.Join(outputDir, rec.Filename)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("cleanup: remove %s: %v", p, err)
		}
	}

	// Remove JSON sidecar.
	sidecarRemoved := false
	if rec.Filename != "" {
		base := strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename))
		candidate := filepath.Join(outputDir, base+".json")
		if err := os.Remove(candidate); err == nil {
			sidecarRemoved = true
		}
	}
	if !sidecarRemoved {
		if entries, err := os.ReadDir(outputDir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				p := filepath.Join(outputDir, e.Name())
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				var tmp struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(data, &tmp) == nil && tmp.ID == id {
					os.Remove(p) //nolint:errcheck
					break
				}
			}
		}
	}

	store.mu.Lock()
	store.deleted[id] = struct{}{}
	delete(store.byID, id)
	filtered := make([]*recordingRecord, 0, len(store.records)-1)
	for _, r := range store.records {
		if r.ID != id {
			filtered = append(filtered, r)
		}
	}
	store.records = filtered
	store.mu.Unlock()

	store.hub.broadcast(sseEvent{
		Event: "recording_deleted",
		Data:  map[string]string{"id": id},
	})

	log.Printf("cleanup: deleted recording %s", id)
}
