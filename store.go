// store.go — thread-safe in-memory store of completed recordings + JSON sidecars
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// recordingRecord holds metadata for one completed WAV recording segment.
type recordingRecord struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	FreqHz      int       `json:"freq_hz"`
	AudioMode   string    `json:"audio_mode"`
	StartedAt   time.Time `json:"started_at"`
	SavedAt     time.Time `json:"saved_at"`
	DurationSec float64   `json:"duration_sec"`
	SampleRate  int       `json:"sample_rate"`
	Channels    int       `json:"channels"`
	Filename    string    `json:"filename"` // WAV filename (basename)
	SNR         SNRStats  `json:"snr"`
}

// recordingStore is a thread-safe in-memory list of completed recordings.
type recordingStore struct {
	mu        sync.RWMutex
	records   []*recordingRecord // newest first
	byID      map[string]*recordingRecord
	deleted   map[string]struct{} // IDs removed by cleanup; prevents re-insertion on restart
	hub       *sseHub
	outputDir string
}

func newRecordingStore(outputDir string, hub *sseHub) *recordingStore {
	s := &recordingStore{
		byID:      make(map[string]*recordingRecord),
		deleted:   make(map[string]struct{}),
		hub:       hub,
		outputDir: outputDir,
	}
	s.loadExisting()
	return s
}

// loadExisting scans outputDir for *.json sidecar files and populates the store.
func (s *recordingStore) loadExisting() {
	entries, err := os.ReadDir(s.outputDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[store] readdir %s: %v", s.outputDir, err)
		}
		return
	}
	var loaded []*recordingRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.outputDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[store] read %s: %v", path, err)
			continue
		}
		if len(data) == 0 {
			continue
		}
		var rec recordingRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			log.Printf("[store] skip corrupt sidecar %s: %v", e.Name(), err)
			continue
		}
		rec.SNR.Sanitise()
		if _, wasDel := s.deleted[rec.ID]; wasDel {
			continue
		}
		// Verify the WAV still exists.
		if _, err := os.Stat(filepath.Join(s.outputDir, rec.Filename)); err != nil {
			continue
		}
		loaded = append(loaded, &rec)
	}
	sort.Slice(loaded, func(i, j int) bool {
		return loaded[i].SavedAt.After(loaded[j].SavedAt)
	})
	s.mu.Lock()
	for _, rec := range loaded {
		s.records = append(s.records, rec)
		s.byID[rec.ID] = rec
	}
	s.mu.Unlock()
	log.Printf("[store] loaded %d existing recordings from %s", len(loaded), s.outputDir)
}

// add inserts a new record and writes its JSON sidecar atomically.
func (s *recordingStore) add(rec *recordingRecord) error {
	if err := os.MkdirAll(s.outputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	base := strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename))
	jsonPath := filepath.Join(s.outputDir, base+".json")
	jdata, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmpPath := jsonPath + ".tmp"
	if err := os.WriteFile(tmpPath, jdata, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, jsonPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	s.mu.Lock()
	s.records = append([]*recordingRecord{rec}, s.records...)
	s.byID[rec.ID] = rec
	s.mu.Unlock()
	return nil
}

// list returns records (newest first), optionally filtered by label.
func (s *recordingStore) list(label string, limit, offset int) []*recordingRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*recordingRecord
	for _, r := range s.records {
		if label != "" && r.Label != label {
			continue
		}
		out = append(out, r)
	}
	if offset >= len(out) {
		return nil
	}
	out = out[offset:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// delete removes a record and its files from disk.
func (s *recordingStore) delete(id string) error {
	s.mu.Lock()
	rec, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("record %s not found", id)
	}
	delete(s.byID, id)
	s.deleted[id] = struct{}{}
	for i, r := range s.records {
		if r.ID == id {
			s.records = append(s.records[:i], s.records[i+1:]...)
			break
		}
	}
	s.mu.Unlock()

	// Remove WAV and JSON sidecar (best-effort).
	for _, name := range []string{
		rec.Filename,
		strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename)) + ".json",
	} {
		if name == "" {
			continue
		}
		_ = os.Remove(filepath.Join(s.outputDir, name))
	}
	return nil
}
