// cleanup.go — background age-based and quota-based cleanup of old recordings
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
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
// quotaConfig — live-updatable overall and per-channel storage quota settings
// ---------------------------------------------------------------------------

// quotaConfig holds the current storage quota policy.
// OverallMB is the maximum total disk usage in MB across all channels (0 = unlimited).
// Channels maps label → max_mb (0 = use OverallMB; if that is also 0 = unlimited).
type quotaConfig struct {
	mu        sync.RWMutex
	OverallMB int64            `json:"overall_mb"`
	Channels  map[string]int64 `json:"channels"`
}

func newQuotaConfig() *quotaConfig {
	return &quotaConfig{Channels: make(map[string]int64)}
}

// getOverall returns the overall quota in bytes (0 = unlimited).
func (qc *quotaConfig) getOverall() int64 {
	qc.mu.RLock()
	defer qc.mu.RUnlock()
	return qc.OverallMB * 1024 * 1024
}

// getForLabel returns the per-channel quota in bytes for a given label (0 = unlimited).
func (qc *quotaConfig) getForLabel(label string) int64 {
	qc.mu.RLock()
	defer qc.mu.RUnlock()
	if mb, ok := qc.Channels[label]; ok {
		return mb * 1024 * 1024
	}
	return 0
}

// setOverall updates the overall quota in MB.
func (qc *quotaConfig) setOverall(mb int64) {
	qc.mu.Lock()
	qc.OverallMB = mb
	qc.mu.Unlock()
}

// setForLabel sets the per-channel quota in MB for a specific label.
func (qc *quotaConfig) setForLabel(label string, mb int64) {
	qc.mu.Lock()
	if qc.Channels == nil {
		qc.Channels = make(map[string]int64)
	}
	if mb == 0 {
		delete(qc.Channels, label)
	} else {
		qc.Channels[label] = mb
	}
	qc.mu.Unlock()
}

// snapshot returns a copy of the config safe for JSON serialisation.
func (qc *quotaConfig) snapshot() (overallMB int64, channels map[string]int64) {
	qc.mu.RLock()
	defer qc.mu.RUnlock()
	ch := make(map[string]int64, len(qc.Channels))
	for k, v := range qc.Channels {
		ch[k] = v
	}
	return qc.OverallMB, ch
}

// load reads quota.json from configPath; silently ignores missing file.
func (qc *quotaConfig) load(configPath string) {
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		log.Printf("quota: load %s: %v", configPath, err)
		return
	}
	qc.mu.Lock()
	defer qc.mu.Unlock()
	if qc.Channels == nil {
		qc.Channels = make(map[string]int64)
	}
	if err := json.Unmarshal(data, qc); err != nil {
		log.Printf("quota: parse %s: %v", configPath, err)
	}
}

// save writes quota.json atomically.
func (qc *quotaConfig) save(configPath string) {
	qc.mu.RLock()
	data, err := json.MarshalIndent(qc, "", "  ")
	qc.mu.RUnlock()
	if err != nil {
		return
	}
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("quota: save %s: %v", configPath, err)
		return
	}
	if err := os.Rename(tmp, configPath); err != nil {
		log.Printf("quota: rename %s: %v", configPath, err)
	}
}

// ---------------------------------------------------------------------------
// Background cleanup worker
// ---------------------------------------------------------------------------

// startCleanup runs a ticker every 5 minutes and:
//  1. Deletes recordings older than the configured retention period per channel.
//  2. Enforces per-channel and overall storage quotas by deleting oldest segments first.
//
// keepDays is the legacy CLI flag value used as the initial default when no
// retention.json exists and keepDays > 0.
// maxStorageMB is the overall storage limit in MB (0 = unlimited); used as the
// initial OverallMB when no quota.json exists and maxStorageMB > 0.
func startCleanup(store *recordingStore, outputDir string, keepDays int, rc *retentionConfig, qc *quotaConfig, maxStorageMB int64) {
	// Apply legacy CLI default if no retention.json was loaded.
	def, _ := rc.snapshot()
	if def == 0 && keepDays > 0 {
		rc.setDefault(keepDays * 24)
	}

	// Apply CLI overall quota if no quota.json was loaded.
	overall, _ := qc.snapshot()
	if overall == 0 && maxStorageMB > 0 {
		qc.setOverall(maxStorageMB)
	}

	log.Printf("cleanup: worker started (check every 5 min)")
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			runAgeCleanup(store, outputDir, rc)
			runQuotaCleanup(store, outputDir, qc)
		}
	}()
}

// startAgeCleanup is kept for backward compatibility; prefer startCleanup.
func startAgeCleanup(store *recordingStore, outputDir string, keepDays int, rc *retentionConfig) {
	startCleanup(store, outputDir, keepDays, rc, newQuotaConfig(), 0)
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

// runQuotaCleanup enforces per-channel and overall storage quotas by deleting
// the oldest segments first until usage is within the configured limits.
func runQuotaCleanup(store *recordingStore, outputDir string, qc *quotaConfig) {
	overallLimit := qc.getOverall()

	// Build a map of label → sorted (oldest-first) records and their WAV sizes.
	store.mu.RLock()
	type recWithSize struct {
		rec  *recordingRecord
		size int64
	}
	byLabel := make(map[string][]recWithSize)
	var allRecs []recWithSize
	for _, r := range store.records {
		p := filepath.Join(outputDir, r.Filename)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		rws := recWithSize{rec: r, size: fi.Size()}
		byLabel[r.Label] = append(byLabel[r.Label], rws)
		allRecs = append(allRecs, rws)
	}
	store.mu.RUnlock()

	// Sort each label's list oldest-first.
	for lbl := range byLabel {
		sort.Slice(byLabel[lbl], func(i, j int) bool {
			return byLabel[lbl][i].rec.SavedAt.Before(byLabel[lbl][j].rec.SavedAt)
		})
	}
	// Sort all records oldest-first for overall quota pass.
	sort.Slice(allRecs, func(i, j int) bool {
		return allRecs[i].rec.SavedAt.Before(allRecs[j].rec.SavedAt)
	})

	deleted := make(map[string]struct{}) // IDs deleted in this pass

	// --- Per-channel quota pass ---
	for lbl, recs := range byLabel {
		limit := qc.getForLabel(lbl)
		if limit <= 0 {
			continue
		}
		var used int64
		for _, rws := range recs {
			used += rws.size
		}
		if used <= limit {
			continue
		}
		log.Printf("cleanup: quota pass — channel %q using %d MB, limit %d MB",
			lbl, used/1024/1024, limit/1024/1024)
		for _, rws := range recs {
			if used <= limit {
				break
			}
			if _, alreadyDel := deleted[rws.rec.ID]; alreadyDel {
				continue
			}
			deleteRecordFiles(store, outputDir, rws.rec)
			deleted[rws.rec.ID] = struct{}{}
			used -= rws.size
		}
	}

	// --- Overall quota pass ---
	if overallLimit <= 0 {
		return
	}
	var totalUsed int64
	for _, rws := range allRecs {
		if _, del := deleted[rws.rec.ID]; !del {
			totalUsed += rws.size
		}
	}
	if totalUsed <= overallLimit {
		return
	}
	log.Printf("cleanup: quota pass — overall using %d MB, limit %d MB",
		totalUsed/1024/1024, overallLimit/1024/1024)
	for _, rws := range allRecs {
		if totalUsed <= overallLimit {
			break
		}
		if _, alreadyDel := deleted[rws.rec.ID]; alreadyDel {
			continue
		}
		deleteRecordFiles(store, outputDir, rws.rec)
		deleted[rws.rec.ID] = struct{}{}
		totalUsed -= rws.size
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
