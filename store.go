// store.go — thread-safe in-memory store of completed recordings + JSON sidecars
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// recordingRecord holds metadata for one completed WAV recording segment.
type recordingRecord struct {
	ID           string    `json:"id"`
	ChannelID    string    `json:"channel_id,omitempty"` // stable channel UUID; links segment to channels.json entry
	SessionID    string    `json:"session_id"`           // shared by all segments in one continuous recording session
	SegmentIndex int       `json:"segment_index"`        // 0-based position within the session
	Label        string    `json:"label"`
	FreqHz       int       `json:"freq_hz"`
	AudioMode    string    `json:"audio_mode"`
	StartedAt    time.Time `json:"started_at"`
	SavedAt      time.Time `json:"saved_at"`
	DurationSec  float64   `json:"duration_sec"`
	SampleRate   int       `json:"sample_rate"`
	Channels     int       `json:"channels"`
	Filename     string    `json:"filename"` // WAV filename (basename)
	SNR          SNRStats  `json:"snr"`
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
// Any *.wav that has no corresponding *.json sidecar (e.g. from a crash) is
// recovered by synthesising a record from the filename and WAV header.
func (s *recordingStore) loadExisting() {
	entries, err := os.ReadDir(s.outputDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[store] readdir %s: %v", s.outputDir, err)
		}
		return
	}

	// Build set of WAV basenames that already have a .json sidecar.
	hasSidecar := make(map[string]struct{})
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") &&
			!strings.HasPrefix(e.Name(), "session_") {
			base := strings.TrimSuffix(e.Name(), ".json")
			hasSidecar[base] = struct{}{}
		}
	}

	var loaded []*recordingRecord

	// Pass 1: load records from existing .json sidecars.
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasPrefix(e.Name(), "session_") {
			continue // session-level sidecar, not a segment
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

	// Pass 2: recover orphaned WAVs (no .json sidecar — likely a crash).
	var recovered int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wav") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".wav")
		if _, ok := hasSidecar[base]; ok {
			continue // already has a sidecar
		}
		wavPath := filepath.Join(s.outputDir, e.Name())
		rec := recoverOrphanedWAV(wavPath, e.Name())
		if rec == nil {
			continue
		}
		// Write the synthesised sidecar so it persists across restarts.
		jsonPath := filepath.Join(s.outputDir, base+".json")
		if jdata, err := json.MarshalIndent(rec, "", "  "); err == nil {
			if werr := writeAtomic(jsonPath, jdata); werr != nil {
				log.Printf("[store] recover: write sidecar %s: %v", jsonPath, werr)
			}
		}
		loaded = append(loaded, rec)
		recovered++
	}
	if recovered > 0 {
		log.Printf("[store] recovered %d orphaned recording(s) from %s", recovered, s.outputDir)
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

// recoverOrphanedWAV synthesises a recordingRecord from a WAV file that has no
// .json sidecar (e.g. the process was killed before closeSegment ran).
// New filenames are pure UUIDs: {uuid}.wav
// Legacy filenames: {YYYYMMDD_HHMMSS}_{label}_{shortID}.wav — label is extracted.
func recoverOrphanedWAV(wavPath, filename string) *recordingRecord {
	base := strings.TrimSuffix(filename, ".wav")

	// Read WAV header to get sample rate, channels, and data size.
	f, err := os.Open(wavPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	hdr := make([]byte, 44)
	if _, err := f.Read(hdr); err != nil {
		return nil
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return nil
	}
	sampleRate := int(binary.LittleEndian.Uint32(hdr[24:28]))
	dataBytes := int(binary.LittleEndian.Uint32(hdr[40:44]))
	channels := int(binary.LittleEndian.Uint16(hdr[22:24]))
	if sampleRate == 0 || channels == 0 {
		return nil
	}

	// If dataBytes is 0 (placeholder header from crash), use actual file size.
	if dataBytes == 0 {
		if fi, err := f.Stat(); err == nil && fi.Size() > 44 {
			dataBytes = int(fi.Size()) - 44
		}
	}
	durationSec := float64(dataBytes) / float64(sampleRate*channels*2)
	if durationSec < 0 {
		durationSec = 0
	}

	fi, _ := f.Stat()

	// Determine segment ID and label.
	// New format: base is a full UUID (36 chars with hyphens).
	// Legacy format: {YYYYMMDD_HHMMSS}_{label}_{shortID8}
	segID := base
	label := ""
	startedAt := time.Time{}
	if len(base) == 36 && strings.Count(base, "-") == 4 {
		// New UUID filename — use file mtime as startedAt approximation.
		if fi != nil {
			startedAt = fi.ModTime().UTC().Add(-time.Duration(durationSec * float64(time.Second)))
		}
	} else {
		// Legacy filename: parse timestamp and label.
		parts := strings.SplitN(base, "_", 3)
		if len(parts) >= 3 {
			tsStr := parts[0] + "_" + parts[1]
			if t, err := time.ParseInLocation("20060102_150405", tsStr, time.UTC); err == nil {
				startedAt = t
			}
			rest := parts[2]
			lastUS := strings.LastIndex(rest, "_")
			if lastUS >= 0 && len(rest)-lastUS-1 == 8 {
				label = rest[:lastUS]
			}
		}
		segID = uuid.New().String() // generate a new ID for legacy files
	}

	savedAt := startedAt.Add(time.Duration(durationSec * float64(time.Second)))
	if fi != nil {
		savedAt = fi.ModTime().UTC()
	}

	return &recordingRecord{
		ID:          segID,
		SessionID:   uuid.New().String(), // synthetic — no session info available
		Label:       label,
		Filename:    filename,
		StartedAt:   startedAt,
		SavedAt:     savedAt,
		DurationSec: durationSec,
		SampleRate:  sampleRate,
		Channels:    channels,
	}
}

// add inserts a new record, writes its per-segment JSON sidecar, and
// updates the session-level JSON sidecar atomically.
func (s *recordingStore) add(rec *recordingRecord) error {
	if err := os.MkdirAll(s.outputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// Per-segment sidecar: {wavbase}.json
	base := strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename))
	jsonPath := filepath.Join(s.outputDir, base+".json")
	jdata, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal segment: %w", err)
	}
	if err := writeAtomic(jsonPath, jdata); err != nil {
		return fmt.Errorf("write segment sidecar: %w", err)
	}

	s.mu.Lock()
	s.records = append([]*recordingRecord{rec}, s.records...)
	s.byID[rec.ID] = rec
	s.mu.Unlock()

	// Session-level sidecar: session_{sessionID}.json — written after unlock
	// so listSessions sees the new record.
	if rec.SessionID != "" {
		if ss := s.getSession(rec.SessionID, ""); ss != nil {
			ssPath := filepath.Join(s.outputDir, "session_"+rec.SessionID+".json")
			if ssData, err := json.MarshalIndent(ss, "", "  "); err == nil {
				_ = writeAtomic(ssPath, ssData)
			}
		}
	}

	return nil
}

// writeAtomic writes data to path via a temp file + rename.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
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

// availableDates returns the set of UTC calendar dates (YYYY-MM-DD) that have
// at least one recording, newest first.
// channelID filters by channel UUID; pass "" for all channels.
func (s *recordingStore) availableDates(channelID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{})
	var order []string
	for _, r := range s.records {
		if channelID != "" && r.ChannelID != channelID {
			continue
		}
		d := r.StartedAt.UTC().Format("2006-01-02")
		if _, ok := seen[d]; !ok {
			seen[d] = struct{}{}
			order = append(order, d)
		}
	}
	return order
}

// dateFilter returns true when the record falls on the given UTC date string
// (YYYY-MM-DD).  An empty date string matches everything (used internally by
// getSession for stream/delete operations; the public /api/sessions endpoint
// always supplies a specific date).
func dateFilter(r *recordingRecord, date string) bool {
	if date == "" {
		return true
	}
	return r.StartedAt.UTC().Format("2006-01-02") == date
}

// sessionSummary is the API representation of one recording session.
type sessionSummary struct {
	SessionID    string             `json:"session_id"`
	ChannelID    string             `json:"channel_id,omitempty"` // stable channel UUID (set when grouped by channel)
	Label        string             `json:"label"`
	FreqHz       int                `json:"freq_hz"`
	AudioMode    string             `json:"audio_mode"`
	StartedAt    time.Time          `json:"started_at"`
	SavedAt      time.Time          `json:"saved_at"`
	DurationSec  float64            `json:"duration_sec"`
	SegmentCount int                `json:"segment_count"`
	SNR          SNRStats           `json:"snr"`
	Segments     []*recordingRecord `json:"segments"`
}

// listByChannelID groups ALL segments for a channel_id UUID into a single
// sessionSummary (ignoring session boundaries). Records without a channel_id
// fall back to grouping by session_id. This is the preferred view for the UI.
// channelID filters by channel UUID (or "" for all channels).
// date is a UTC calendar date string "YYYY-MM-DD"; pass "" only for internal
// calls (stream/delete) that need all dates — the public API always passes a date.
func (s *recordingStore) listByChannelID(channelID string, limit, offset int, date string) ([]*sessionSummary, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]*sessionSummary) // keyed by channel_id (or fallback key)
	var order []string
	for _, r := range s.records {
		if channelID != "" && r.ChannelID != channelID {
			continue
		}
		if !dateFilter(r, date) {
			continue
		}
		// Group key: prefer channel_id UUID; fall back to session_id for old records.
		key := r.ChannelID
		if key == "" {
			key = r.SessionID
		}
		if key == "" {
			key = "solo_" + r.ID
		}
		if _, ok := seen[key]; !ok {
			ss := &sessionSummary{
				// SessionID is set to the channel UUID so existing /api/sessions/{id}
				// endpoints work transparently when the frontend passes channel_id.
				SessionID: key,
				ChannelID: r.ChannelID,
				Label:     r.Label,
				FreqHz:    r.FreqHz,
				AudioMode: r.AudioMode,
			}
			seen[key] = ss
			order = append(order, key)
		}
		ss := seen[key]
		// Keep label up-to-date (most recent segment wins — handles renames).
		if r.Label != "" {
			ss.Label = r.Label
		}
		ss.Segments = append(ss.Segments, r)
		ss.DurationSec += r.DurationSec
	}

	for _, key := range order {
		ss := seen[key]
		// Sort all segments by start time (oldest first).
		sort.Slice(ss.Segments, func(i, j int) bool {
			return ss.Segments[i].StartedAt.Before(ss.Segments[j].StartedAt)
		})
		ss.SegmentCount = len(ss.Segments)
		if len(ss.Segments) > 0 {
			ss.StartedAt = ss.Segments[0].StartedAt
			ss.SavedAt = ss.Segments[len(ss.Segments)-1].SavedAt
		}
		// Merge SNR across all segments (weighted average by sample count).
		var totalCount int
		var weightedSum float32
		var minDB, maxDB float32
		first := true
		for _, seg := range ss.Segments {
			if seg.SNR.Count == 0 {
				continue
			}
			weightedSum += seg.SNR.AvgDB * float32(seg.SNR.Count)
			totalCount += seg.SNR.Count
			if first || seg.SNR.MinDB < minDB {
				minDB = seg.SNR.MinDB
			}
			if first || seg.SNR.MaxDB > maxDB {
				maxDB = seg.SNR.MaxDB
			}
			first = false
		}
		if totalCount > 0 {
			ss.SNR = SNRStats{
				AvgDB: weightedSum / float32(totalCount),
				MinDB: minDB,
				MaxDB: maxDB,
				Count: totalCount,
			}
		}
	}

	total := len(order)
	if offset >= total {
		return nil, total
	}
	order = order[offset:]
	if limit > 0 && len(order) > limit {
		order = order[:limit]
	}
	out := make([]*sessionSummary, 0, len(order))
	for _, key := range order {
		out = append(out, seen[key])
	}
	return out, total
}

// listSessions groups completed segments by session_id and returns summaries
// newest-first. Segments within each session are ordered by segment_index.
// date is an optional UTC calendar date string "YYYY-MM-DD" (or "" for all dates).
func (s *recordingStore) listSessions(label string, limit, offset int, date string) ([]*sessionSummary, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build ordered list of session IDs (preserving newest-first order of first
	// segment seen, which is the most-recently-saved segment of each session).
	// Segments with an empty session_id (recorded before the session feature was
	// added) are each treated as their own single-segment session keyed by ID.
	seen := make(map[string]*sessionSummary)
	var order []string
	for _, r := range s.records {
		if label != "" && r.Label != label {
			continue
		}
		if !dateFilter(r, date) {
			continue
		}
		key := r.SessionID
		if key == "" {
			key = "solo_" + r.ID // treat as standalone session
		}
		if _, ok := seen[key]; !ok {
			ss := &sessionSummary{
				SessionID: r.SessionID,
				Label:     r.Label,
				FreqHz:    r.FreqHz,
				AudioMode: r.AudioMode,
			}
			seen[key] = ss
			order = append(order, key)
		}
		ss := seen[key]
		ss.Segments = append(ss.Segments, r)
		ss.DurationSec += r.DurationSec
	}

	// Finalise each session: sort segments, compute time bounds, merge SNR.
	for _, id := range order {
		ss := seen[id]
		sort.Slice(ss.Segments, func(i, j int) bool {
			return ss.Segments[i].SegmentIndex < ss.Segments[j].SegmentIndex
		})
		ss.SegmentCount = len(ss.Segments)
		if len(ss.Segments) > 0 {
			ss.StartedAt = ss.Segments[0].StartedAt
			ss.SavedAt = ss.Segments[len(ss.Segments)-1].SavedAt
		}
		// Merge SNR across segments (weighted average by sample count).
		var totalCount int
		var weightedSum float32
		var minDB, maxDB float32
		first := true
		for _, seg := range ss.Segments {
			if seg.SNR.Count == 0 {
				continue
			}
			weightedSum += seg.SNR.AvgDB * float32(seg.SNR.Count)
			totalCount += seg.SNR.Count
			if first || seg.SNR.MinDB < minDB {
				minDB = seg.SNR.MinDB
			}
			if first || seg.SNR.MaxDB > maxDB {
				maxDB = seg.SNR.MaxDB
			}
			first = false
		}
		if totalCount > 0 {
			ss.SNR = SNRStats{
				AvgDB: weightedSum / float32(totalCount),
				MinDB: minDB,
				MaxDB: maxDB,
				Count: totalCount,
			}
		}
	}

	total := len(order)
	if offset >= total {
		return nil, total
	}
	order = order[offset:]
	if limit > 0 && len(order) > limit {
		order = order[:limit]
	}
	out := make([]*sessionSummary, 0, len(order))
	for _, id := range order {
		out = append(out, seen[id])
	}
	return out, total
}

// getSession returns the sessionSummary for a single session ID or channel UUID, or nil.
// It first searches by session_id (exact session), then falls back to searching
// the channel-grouped view (where SessionID == channel_id UUID).
// date is an optional UTC calendar date filter (YYYY-MM-DD); "" means all dates.
func (s *recordingStore) getSession(sessionID string, date string) *sessionSummary {
	sessions, _ := s.listSessions("", 0, 0, date)
	for _, ss := range sessions {
		if ss.SessionID == sessionID {
			return ss
		}
	}
	// Fall back: treat sessionID as a channel UUID and return the channel-grouped summary.
	channelSessions, _ := s.listByChannelID(sessionID, 0, 0, date)
	for _, ss := range channelSessions {
		if ss.SessionID == sessionID {
			return ss
		}
	}
	return nil
}

// delete removes a record and its files from disk.
func (s *recordingStore) delete(id string) error {
	s.mu.Lock()
	rec, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("record %s not found", id)
	}
	sessionID := rec.SessionID
	delete(s.byID, id)
	s.deleted[id] = struct{}{}
	for i, r := range s.records {
		if r.ID == id {
			s.records = append(s.records[:i], s.records[i+1:]...)
			break
		}
	}
	// Check if any sibling segments remain in this session.
	var sessionHasRemaining bool
	for _, r := range s.records {
		if r.SessionID == sessionID {
			sessionHasRemaining = true
			break
		}
	}
	s.mu.Unlock()

	// Remove per-segment WAV and JSON sidecar (best-effort).
	for _, name := range []string{
		rec.Filename,
		strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename)) + ".json",
	} {
		if name == "" {
			continue
		}
		_ = os.Remove(filepath.Join(s.outputDir, name))
	}

	// Update or remove the session-level sidecar.
	if sessionID != "" {
		ssPath := filepath.Join(s.outputDir, "session_"+sessionID+".json")
		if sessionHasRemaining {
			if ss := s.getSession(sessionID, ""); ss != nil {
				if ssData, err := json.MarshalIndent(ss, "", "  "); err == nil {
					_ = writeAtomic(ssPath, ssData)
				}
			}
		} else {
			_ = os.Remove(ssPath)
		}
	}

	return nil
}
