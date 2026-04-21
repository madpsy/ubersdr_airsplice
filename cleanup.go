// cleanup.go — background age-based cleanup of old recordings
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const cleanupInterval = 5 * time.Minute

// startAgeCleanup runs a ticker every 5 minutes and deletes ALL recordings
// older than keepDays.  keepDays == 0 disables the worker.
func startAgeCleanup(store *recordingStore, outputDir string, keepDays int) {
	if keepDays <= 0 {
		return
	}
	log.Printf("cleanup: age worker started (delete recordings after %d day(s), check every 5 min)", keepDays)
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			runAgeCleanup(store, outputDir, keepDays)
		}
	}()
}

func runAgeCleanup(store *recordingStore, outputDir string, keepDays int) {
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)

	store.mu.RLock()
	var candidates []*recordingRecord
	for _, r := range store.records {
		if r.SavedAt.Before(cutoff) {
			candidates = append(candidates, r)
		}
	}
	store.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}
	log.Printf("cleanup: age pass — %d recording(s) older than %d day(s)", len(candidates), keepDays)
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
