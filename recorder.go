// recorder.go — per-channel audio recorder: wires instance → WAV file writer
//
// Session model:
//   - A "session" starts when the channel connects and begins recording.
//   - Each session has a UUID (sessionID) shared by all its segments.
//   - When the segment rotation timer fires, a new segment is opened within
//     the same session (segmentIndex increments).
//   - When the UberSDR connection drops and reconnects, a NEW session UUID is
//     generated so the gap is visible in the UI.
//
// Per-segment files:
//
//	{ts}_{label}_{shortID}.wav          — PCM audio
//	{ts}_{label}_{shortID}.json         — segment metadata (written on close)
//	{ts}_{label}_{shortID}.jsonl        — telemetry log: one JSON object per line,
//	                                      appended every ~10 s while recording.
//	                                      Each line: {"t":"<ISO>","snr":{...},"level_dbfs":<f32>}
//
// Smart Record (VOX-style SNR gate):
//
//	When SmartRecordConfig.Enabled is true the recorder does NOT write audio
//	continuously.  Instead it monitors the rolling SNR from the snrAccumulator
//	and applies a two-threshold hysteresis gate:
//
//	  • IDLE → ARMED: SNR rises above StartThreshDB and stays there for
//	    StartHoldSec seconds.  The recorder then opens a new WAV segment and
//	    enters RECORDING state.
//	  • RECORDING → TAIL: SNR falls below StopThreshDB.  A countdown of
//	    StopHoldSec seconds begins.  If SNR rises again before the countdown
//	    expires the recorder stays in RECORDING state.
//	  • TAIL → IDLE: countdown expires.  The current segment is closed.
//
//	The gate is evaluated every gateTickInterval (250 ms) using the most
//	recent SNR samples from snrAccumulator.peekLatest().
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// gateTickInterval is how often the smart-record SNR gate is evaluated.
const gateTickInterval = 250 * time.Millisecond

// gateState is the internal state machine for the smart-record VOX gate.
type gateState int

const (
	gateIdle      gateState = iota // not recording; waiting for SNR to rise
	gateArming                     // SNR above start threshold; counting hold time
	gateRecording                  // actively writing audio to disk
	gateTail                       // SNR below stop threshold; counting tail time
)

// recChannel owns one UberSDR instance and writes its audio to WAV files.
// When segmentSecs > 0 it rotates to a new file every segmentSecs seconds.
// When segmentSecs == 0 it writes one continuous file until stopped.
type recChannel struct {
	inst        *instance
	store       *recordingStore
	hub         *sseHub
	segmentSecs int
	label       string
	channelID   string // stable UUID from channels.json; never changes on rename

	mu           sync.Mutex
	current      *activeRecording // nil when not recording
	sessionID    string           // UUID shared by all segments in one continuous session
	segmentIndex int              // increments with each rotation within a session

	// Smart record (VOX gate) — protected by srMu so it can be updated live.
	srMu sync.RWMutex
	sr   SmartRecordConfig

	// Schedule — protected by schedMu so it can be updated live.
	schedMu sync.RWMutex
	sched   ScheduleConfig

	// Gate state — only accessed from the run() goroutine (no lock needed).
	gState     gateState
	gHoldStart time.Time // when the current hold period began
	// pcmBuf accumulates audio chunks while the gate is arming so they are
	// written to the segment once recording actually starts (pre-roll).
	pcmBuf [][]byte
}

// activeRecording holds state for the currently-open WAV file.
type activeRecording struct {
	id           string
	sessionID    string
	segmentIndex int
	label        string
	channelID    string // stable channel UUID
	freqHz       int
	audioMode    string
	file         *os.File
	jsonlFile    *os.File // telemetry log; nil if open failed
	path         string
	startedAt    time.Time
	sampleRate   int
	channels     int
	bytesWritten int64    // PCM bytes written (excludes header)
	snrMerged    SNRStats // running weighted merge of all drained 60s windows
}

// mergeSNRInto merges src into dst using weighted average by sample count.
func mergeSNRInto(dst *SNRStats, src SNRStats) {
	if src.Count == 0 {
		return
	}
	if dst.Count == 0 {
		*dst = src
		return
	}
	total := dst.Count + src.Count
	dst.AvgDB = (dst.AvgDB*float32(dst.Count) + src.AvgDB*float32(src.Count)) / float32(total)
	if src.MinDB < dst.MinDB {
		dst.MinDB = src.MinDB
	}
	if src.MaxDB > dst.MaxDB {
		dst.MaxDB = src.MaxDB
	}
	dst.BasebandAvg = (dst.BasebandAvg*float32(dst.Count) + src.BasebandAvg*float32(src.Count)) / float32(total)
	dst.NoiseAvg = (dst.NoiseAvg*float32(dst.Count) + src.NoiseAvg*float32(src.Count)) / float32(total)
	dst.Count = total
}

// telemetryEntry is one line in the .jsonl telemetry file.
type telemetryEntry struct {
	T         string   `json:"t"`
	SNR       SNRStats `json:"snr"`
	LevelDBFS float32  `json:"level_dbfs"`
}

func newRecChannel(inst *instance, store *recordingStore, hub *sseHub, segmentSecs int, channelID string, sr SmartRecordConfig, sched ScheduleConfig) *recChannel {
	return &recChannel{
		inst:        inst,
		store:       store,
		hub:         hub,
		segmentSecs: segmentSecs,
		label:       inst.label,
		channelID:   channelID,
		sr:          sr,
		sched:       sched,
	}
}

// setSmartRecord atomically replaces the smart-record config.
func (c *recChannel) setSmartRecord(sr SmartRecordConfig) {
	c.srMu.Lock()
	c.sr = sr
	c.srMu.Unlock()
	log.Printf("[%s] smart record updated: enabled=%v start=%.1fdB/%.1fs stop=%.1fdB/%.1fs maxLen=%.1fmin",
		c.label, sr.Enabled, sr.StartThreshDB, sr.StartHoldSec, sr.StopThreshDB, sr.StopHoldSec, sr.MaxRecordMins)
}

// getSmartRecord returns a copy of the current smart-record config.
func (c *recChannel) getSmartRecord() SmartRecordConfig {
	c.srMu.RLock()
	defer c.srMu.RUnlock()
	return c.sr
}

// setSchedule atomically replaces the schedule config.
func (c *recChannel) setSchedule(sched ScheduleConfig) {
	c.schedMu.Lock()
	c.sched = sched
	c.schedMu.Unlock()
	log.Printf("[%s] schedule updated: enabled=%v timezone=%q rules=%d",
		c.label, sched.Enabled, sched.Timezone, len(sched.Rules))
}

// getSchedule returns a copy of the current schedule config.
func (c *recChannel) getSchedule() ScheduleConfig {
	c.schedMu.RLock()
	defer c.schedMu.RUnlock()
	return c.sched
}

// scheduleTickInterval is how often the schedule gate is evaluated.
const scheduleTickInterval = 10 * time.Second

// run starts the channel and blocks until ctx is cancelled.
func (c *recChannel) run(ctx context.Context) {
	go c.inst.start(ctx)

	for {
		// Wait for first audio packet (or reconnect after a gap).
		var firstChunk []byte
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-c.inst.AudioCh:
			if !ok {
				return
			}
			firstChunk = chunk
		}

		// Each (re)connect starts a fresh session.
		c.mu.Lock()
		c.sessionID = uuid.New().String()
		c.segmentIndex = 0
		c.mu.Unlock()

		c.inst.streamMu.RLock()
		sampleRate := c.inst.streamSampleRate
		c.inst.streamMu.RUnlock()

		if sampleRate == 0 {
			sampleRate = 8000
		}
		// Audio is always delivered as mono S16LE after downmix in ubersdr.go
		numChannels := 1

		log.Printf("[%s] new session %s: %d Hz, %d ch, segment=%ds",
			c.label, c.sessionID[:8], sampleRate, numChannels, c.segmentSecs)

		// Reset gate state for the new session.
		c.gState = gateIdle
		c.gHoldStart = time.Time{}
		c.pcmBuf = nil

		sr := c.getSmartRecord()
		sched := c.getSchedule()

		// Determine whether to open a segment immediately:
		//   - Schedule enabled: only open if schedule is currently active.
		//   - VOX enabled: never open immediately (gate handles it).
		//   - Otherwise (continuous): open immediately.
		if sched.Enabled {
			if sched.Active(time.Now()) {
				if !sr.Enabled {
					if err := c.openSegment(sampleRate, numChannels); err != nil {
						log.Printf("[%s] open segment: %v", c.label, err)
						return
					}
				}
				// If VOX is also enabled, the gate ticker will open the segment
				// when SNR crosses the threshold (within the schedule window).
			} else {
				log.Printf("[%s] schedule active but window not open — waiting", c.label)
			}
		} else if !sr.Enabled {
			// Normal (continuous) recording: open the first segment immediately.
			if err := c.openSegment(sampleRate, numChannels); err != nil {
				log.Printf("[%s] open segment: %v", c.label, err)
				return
			}
		} else {
			log.Printf("[%s] smart record active: start≥%.1fdB/%.1fs stop<%.1fdB/%.1fs",
				c.label, sr.StartThreshDB, sr.StartHoldSec, sr.StopThreshDB, sr.StopHoldSec)
		}

		// Segment rotation ticker (disabled when segmentSecs == 0).
		var rotateCh <-chan time.Time
		var rotateTicker *time.Ticker
		if c.segmentSecs > 0 {
			rotateTicker = time.NewTicker(time.Duration(c.segmentSecs) * time.Second)
			rotateCh = rotateTicker.C
		}

		// Telemetry ticker — snapshot SNR + level every 10 s.
		telemTicker := time.NewTicker(10 * time.Second)

		// Smart-record gate ticker — evaluated every gateTickInterval.
		gateTicker := time.NewTicker(gateTickInterval)

		// Schedule ticker — evaluated every scheduleTickInterval.
		schedTicker := time.NewTicker(scheduleTickInterval)

		// Feed the first chunk.
		c.handleChunk(firstChunk, sampleRate, numChannels)

		reconnect := false
	loop:
		for {
			select {
			case <-ctx.Done():
				if rotateTicker != nil {
					rotateTicker.Stop()
				}
				telemTicker.Stop()
				gateTicker.Stop()
				schedTicker.Stop()
				c.closeSegment()
				return

			case <-rotateCh:
				// Only rotate when actually recording (smart record / schedule may be idle).
				c.mu.Lock()
				isRecording := c.current != nil
				c.mu.Unlock()
				if isRecording {
					if err := c.rotateSegment(sampleRate, numChannels); err != nil {
						log.Printf("[%s] rotate segment: %v", c.label, err)
					}
				}

			case <-telemTicker.C:
				c.appendTelemetry()

			case <-gateTicker.C:
				c.tickGate(sampleRate, numChannels)

			case <-schedTicker.C:
				c.tickSchedule(sampleRate, numChannels)

			case chunk, ok := <-c.inst.AudioCh:
				if !ok {
					// Channel closed — connection dropped.
					reconnect = true
					break loop
				}
				c.handleChunk(chunk, sampleRate, numChannels)
			}
		}

		if rotateTicker != nil {
			rotateTicker.Stop()
		}
		telemTicker.Stop()
		gateTicker.Stop()
		schedTicker.Stop()
		c.closeSegment()

		if !reconnect {
			return
		}
		// Wait briefly before trying to read the next connect burst.
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// tickSchedule evaluates the schedule gate and opens/closes segments as needed.
// Called every scheduleTickInterval from the run() loop.
func (c *recChannel) tickSchedule(sampleRate, numChannels int) {
	sched := c.getSchedule()
	if !sched.Enabled {
		return
	}

	sr := c.getSmartRecord()
	active := sched.Active(time.Now())

	c.mu.Lock()
	isRecording := c.current != nil
	c.mu.Unlock()

	if active && !isRecording {
		// Schedule window just opened.
		if !sr.Enabled {
			// Open segment directly (continuous within window).
			log.Printf("[%s] schedule: window opened — starting recording", c.label)
			if err := c.openSegment(sampleRate, numChannels); err != nil {
				log.Printf("[%s] schedule: open segment: %v", c.label, err)
			}
		}
		// If VOX is also enabled, the gate ticker will open the segment
		// when SNR crosses the threshold.
	} else if !active && isRecording {
		// Schedule window just closed — stop recording.
		log.Printf("[%s] schedule: window closed — stopping recording", c.label)
		c.closeSegment()
		// Also reset VOX gate so it doesn't re-open outside the window.
		c.gState = gateIdle
		c.pcmBuf = nil
	}
}

// handleChunk routes an incoming PCM chunk to the write path or the pre-roll
// buffer depending on the current smart-record gate state.
func (c *recChannel) handleChunk(pcm []byte, sampleRate, numChannels int) {
	sr := c.getSmartRecord()
	sched := c.getSchedule()

	// If schedule is enabled and the window is not active, discard audio.
	if sched.Enabled && !sched.Active(time.Now()) {
		return
	}

	if !sr.Enabled {
		// Normal continuous recording (or continuous within a schedule window).
		c.write(pcm)
		return
	}

	switch c.gState {
	case gateIdle:
		// Discard audio while idle — the gate tick will transition to arming
		// and reset pcmBuf at that point, so we don't buffer anything here.

	case gateArming:
		// Accumulate every chunk from the moment arming started so the full
		// StartHoldSec window is captured as pre-roll when recording opens.
		c.pcmBuf = append(c.pcmBuf, pcm)

	case gateRecording, gateTail:
		// Write directly to the open segment.
		c.write(pcm)
	}
}

// tickGate evaluates the SNR gate and transitions state as needed.
// Called every gateTickInterval from the run() loop.
func (c *recChannel) tickGate(sampleRate, numChannels int) {
	sr := c.getSmartRecord()
	if !sr.Enabled {
		return
	}

	// Use the most recent ~2 samples (≈200 ms at 100 ms packet rate) for a
	// near-instantaneous SNR reading.
	snr := c.inst.snrAccum.peekLatest(2)
	now := time.Now()

	switch c.gState {
	case gateIdle:
		if snr.Count > 0 && snr.AvgDB >= sr.StartThreshDB {
			// SNR crossed the start threshold — begin arming.
			// Reset pcmBuf so we capture audio from exactly this moment.
			c.gState = gateArming
			c.gHoldStart = now
			c.pcmBuf = nil // start fresh; handleChunk will fill from here
			log.Printf("[%s] smart record: arming (SNR %.1f dB ≥ %.1f dB)", c.label, snr.AvgDB, sr.StartThreshDB)
		}

	case gateArming:
		if snr.Count == 0 || snr.AvgDB < sr.StartThreshDB {
			// SNR dropped before hold expired — back to idle.
			c.gState = gateIdle
			c.pcmBuf = nil
			log.Printf("[%s] smart record: disarmed (SNR %.1f dB < %.1f dB)", c.label, snr.AvgDB, sr.StartThreshDB)
			return
		}
		if now.Sub(c.gHoldStart) >= time.Duration(float64(sr.StartHoldSec)*float64(time.Second)) {
			// Hold time satisfied — open a segment and flush the entire pre-roll
			// buffer (which contains audio from the moment arming started, i.e.
			// the full StartHoldSec window).
			log.Printf("[%s] smart record: RECORDING (SNR %.1f dB, held %.1fs, pre-roll %d chunks)",
				c.label, snr.AvgDB, sr.StartHoldSec, len(c.pcmBuf))
			if err := c.openSegment(sampleRate, numChannels); err != nil {
				log.Printf("[%s] smart record: open segment: %v", c.label, err)
				c.gState = gateIdle
				c.pcmBuf = nil
				return
			}
			// Flush pre-roll buffer — this writes the audio from the start of
			// the arming period so the recording begins at the signal onset.
			for _, chunk := range c.pcmBuf {
				c.write(chunk)
			}
			c.pcmBuf = nil
			c.gState = gateRecording
		}

	case gateRecording:
		// Check max recording length limit (if configured).
		if sr.MaxRecordMins > 0 {
			c.mu.Lock()
			var elapsed time.Duration
			if c.current != nil {
				elapsed = now.Sub(c.current.startedAt)
			}
			c.mu.Unlock()
			if elapsed >= time.Duration(float64(sr.MaxRecordMins)*float64(time.Minute)) {
				// Max length reached — close the segment and go idle.
				// The signal must drop below the stop threshold and re-arm before
				// a new recording can start (prevents constant noise from looping).
				log.Printf("[%s] smart record: max length %.1f min reached — stopping, returning to idle", c.label, sr.MaxRecordMins)
				c.closeSegment()
				c.gState = gateIdle
				c.pcmBuf = nil
				return
			}
		}
		if snr.Count > 0 && snr.AvgDB < sr.StopThreshDB {
			// SNR dropped below stop threshold — start tail countdown.
			c.gState = gateTail
			c.gHoldStart = now
			log.Printf("[%s] smart record: tail started (SNR %.1f dB < %.1f dB)", c.label, snr.AvgDB, sr.StopThreshDB)
		}

	case gateTail:
		if snr.Count > 0 && snr.AvgDB >= sr.StopThreshDB {
			// SNR recovered — back to recording.
			c.gState = gateRecording
			log.Printf("[%s] smart record: tail cancelled (SNR %.1f dB ≥ %.1f dB)", c.label, snr.AvgDB, sr.StopThreshDB)
			return
		}
		if now.Sub(c.gHoldStart) >= time.Duration(float64(sr.StopHoldSec)*float64(time.Second)) {
			// Tail expired — close the segment and go idle.
			log.Printf("[%s] smart record: IDLE (tail expired, SNR %.1f dB)", c.label, snr.AvgDB)
			c.closeSegment()
			c.gState = gateIdle
			c.pcmBuf = nil
		}
	}
}

// write appends PCM bytes to the current segment.
func (c *recChannel) write(pcm []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || len(pcm) == 0 {
		return
	}
	n, err := c.current.file.Write(pcm)
	if err != nil {
		log.Printf("[%s] write: %v", c.label, err)
		return
	}
	c.current.bytesWritten += int64(n)
}

// appendTelemetry drains the last ~10s of SNR samples, writes one line to the
// .jsonl telemetry file, merges the drained stats into the running segment
// total, and refreshes the draft .json sidecar.
func (c *recChannel) appendTelemetry() {
	c.mu.Lock()
	rec := c.current
	c.mu.Unlock()
	if rec == nil {
		return
	}

	// drain() gives us exactly the samples accumulated since the last drain()
	// call (i.e. the last ~10 s), which is what we want for the .jsonl entry.
	snr := c.inst.snrAccum.drain()
	now := time.Now().UTC()

	// Merge this window into the running segment total.
	c.mu.Lock()
	if c.current == rec {
		mergeSNRInto(&rec.snrMerged, snr)
	}
	c.mu.Unlock()

	// Append telemetry line.
	if rec.jsonlFile != nil {
		entry := telemetryEntry{
			T:         now.Format(time.RFC3339),
			SNR:       snr,
			LevelDBFS: snr.BasebandAvg,
		}
		line, err := json.Marshal(entry)
		if err == nil {
			line = append(line, '\n')
			c.mu.Lock()
			if c.current == rec {
				_, _ = rec.jsonlFile.Write(line)
				_ = rec.jsonlFile.Sync()
			}
			c.mu.Unlock()
		}
	}

	// Refresh draft .json sidecar with current duration and merged SNR.
	c.mu.Lock()
	if c.current == rec {
		dur := 0.0
		if rec.sampleRate > 0 && rec.channels > 0 {
			dur = float64(rec.bytesWritten) / float64(rec.sampleRate*rec.channels*2)
		}
		merged := rec.snrMerged
		draft := &recordingRecord{
			ID:           rec.id,
			ChannelID:    rec.channelID,
			SessionID:    rec.sessionID,
			SegmentIndex: rec.segmentIndex,
			Label:        rec.label,
			FreqHz:       rec.freqHz,
			AudioMode:    rec.audioMode,
			StartedAt:    rec.startedAt,
			SavedAt:      now,
			DurationSec:  dur,
			SampleRate:   rec.sampleRate,
			Channels:     rec.channels,
			Filename:     rec.id + ".wav",
			SNR:          merged,
		}
		c.mu.Unlock()
		base := filepath.Join(c.store.outputDir, rec.id+".json")
		if jdata, err := json.MarshalIndent(draft, "", "  "); err == nil {
			_ = writeAtomic(base, jdata)
		}
	} else {
		c.mu.Unlock()
	}
}

// openSegment creates a new WAV file and writes a placeholder header.
func (c *recChannel) openSegment(sampleRate, channels int) error {
	if err := os.MkdirAll(c.store.outputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	id := uuid.New().String()
	ts := time.Now().UTC()

	c.mu.Lock()
	idx := c.segmentIndex
	sessID := c.sessionID
	c.mu.Unlock()

	// Filename is the segment UUID — all human-readable metadata is in the .json sidecar.
	base := id
	wavPath := filepath.Join(c.store.outputDir, base+".wav")
	jsonlPath := filepath.Join(c.store.outputDir, base+".jsonl")

	f, err := os.Create(wavPath)
	if err != nil {
		return fmt.Errorf("create wav %s: %w", wavPath, err)
	}

	// Write a placeholder WAV header (will be finalised on close).
	if err := writeWAVHeader(f, sampleRate, channels, 0); err != nil {
		f.Close()
		_ = os.Remove(wavPath)
		return fmt.Errorf("write header: %w", err)
	}

	// Open JSONL telemetry file (non-fatal if it fails).
	jf, err := os.Create(jsonlPath)
	if err != nil {
		log.Printf("[%s] warning: could not create telemetry file %s: %v", c.label, jsonlPath, err)
		jf = nil
	}

	rec := &activeRecording{
		id:           id,
		sessionID:    sessID,
		segmentIndex: idx,
		label:        c.label,
		channelID:    c.channelID,
		freqHz:       c.inst.freqHz,
		audioMode:    c.inst.audioMode,
		file:         f,
		jsonlFile:    jf,
		path:         wavPath,
		startedAt:    ts,
		sampleRate:   sampleRate,
		channels:     channels,
	}
	c.mu.Lock()
	c.current = rec
	c.mu.Unlock()

	// Write a draft .json sidecar immediately so the segment is recoverable
	// even if the process is killed before closeSegment runs.
	draftPath := filepath.Join(c.store.outputDir, base+".json")
	draft := &recordingRecord{
		ID:           id,
		ChannelID:    c.channelID,
		SessionID:    sessID,
		SegmentIndex: idx,
		Label:        c.label,
		FreqHz:       c.inst.freqHz,
		AudioMode:    c.inst.audioMode,
		StartedAt:    ts,
		SavedAt:      ts,
		DurationSec:  0,
		SampleRate:   sampleRate,
		Channels:     channels,
		Filename:     base + ".wav",
	}
	if jdata, err := json.MarshalIndent(draft, "", "  "); err == nil {
		if werr := writeAtomic(draftPath, jdata); werr != nil {
			log.Printf("[%s] warning: could not write draft sidecar: %v", c.label, werr)
		}
	}

	log.Printf("[%s] opened segment %s (session %s, idx %d)", c.label, base, sessID[:8], idx)

	c.hub.broadcast(sseEvent{
		Event: "recording_started",
		Data: map[string]interface{}{
			"label":         c.label,
			"freq_hz":       c.inst.freqHz,
			"id":            id,
			"session_id":    sessID,
			"segment_index": idx,
		},
	})

	return nil
}

// closeSegment finalises the WAV header and saves the record.
func (c *recChannel) closeSegment() {
	c.mu.Lock()
	rec := c.current
	sessID := c.sessionID
	idx := c.segmentIndex
	c.current = nil
	c.mu.Unlock()

	if rec == nil {
		return
	}

	// Finalise WAV header with actual data size.
	if _, err := rec.file.Seek(0, 0); err == nil {
		_ = writeWAVHeader(rec.file, rec.sampleRate, rec.channels, int(rec.bytesWritten))
	}
	rec.file.Close()

	if rec.jsonlFile != nil {
		rec.jsonlFile.Close()
	}

	fname := filepath.Base(rec.path)
	durationSecs := 0.0
	if rec.sampleRate > 0 && rec.channels > 0 {
		durationSecs = float64(rec.bytesWritten) / float64(rec.sampleRate*rec.channels*2)
	}

	// Drain any remaining samples since the last telemetry tick, then merge
	// with the running segment total to get the full-segment SNR.
	tail := c.inst.DrainSNR()
	merged := rec.snrMerged
	mergeSNRInto(&merged, tail)

	// Write a final telemetry entry for any samples not yet flushed by the
	// 10-second ticker.  This is especially important for short VOX activations
	// that close before the first ticker fires.
	if tail.Count > 0 && rec.jsonlFile != nil {
		entry := telemetryEntry{
			T:         time.Now().UTC().Format(time.RFC3339),
			SNR:       tail,
			LevelDBFS: tail.BasebandAvg,
		}
		if line, err := json.Marshal(entry); err == nil {
			line = append(line, '\n')
			_, _ = rec.jsonlFile.Write(line)
		}
	}

	r := &recordingRecord{
		ID:           rec.id,
		ChannelID:    rec.channelID,
		SessionID:    sessID,
		SegmentIndex: idx,
		Label:        c.label,
		FreqHz:       c.inst.freqHz,
		AudioMode:    rec.audioMode,
		StartedAt:    rec.startedAt,
		SavedAt:      time.Now().UTC(),
		DurationSec:  durationSecs,
		SampleRate:   rec.sampleRate,
		Channels:     rec.channels,
		Filename:     fname,
		SNR:          merged,
	}

	if err := c.store.add(r); err != nil {
		log.Printf("[%s] store.add: %v", c.label, err)
	}

	log.Printf("[%s] closed segment %s (session %s idx %d, %.1fs, %.1f kB)",
		c.label, fname, sessID[:8], idx, durationSecs, float64(rec.bytesWritten)/1024)

	c.hub.broadcast(sseEvent{
		Event: "recording_saved",
		Data:  r,
	})
}

// rotateSegment closes the current segment and opens a new one within the same session.
func (c *recChannel) rotateSegment(sampleRate, channels int) error {
	c.closeSegment()
	c.mu.Lock()
	c.segmentIndex++
	c.mu.Unlock()
	return c.openSegment(sampleRate, channels)
}

// statusSnapshot returns a JSON-friendly status map for this channel.
func (c *recChannel) statusSnapshot() map[string]interface{} {
	snap := c.inst.statusSnapshot()
	snap["channel_id"] = c.channelID
	c.mu.Lock()
	if c.current != nil {
		snap["recording"] = true
		snap["segment_started_at"] = c.current.startedAt
		snap["segment_bytes"] = c.current.bytesWritten
		snap["session_id"] = c.sessionID
		snap["segment_index"] = c.segmentIndex
	} else {
		snap["recording"] = false
	}
	c.mu.Unlock()

	// Smart record state.
	sr := c.getSmartRecord()
	snap["smart_record"] = sr
	if sr.Enabled {
		var gateStr string
		switch c.gState {
		case gateIdle:
			gateStr = "idle"
		case gateArming:
			gateStr = "arming"
		case gateRecording:
			gateStr = "recording"
		case gateTail:
			gateStr = "tail"
		}
		snap["smart_record_gate"] = gateStr
	}

	// Schedule state.
	sched := c.getSchedule()
	snap["schedule"] = sched
	if sched.Enabled {
		now := time.Now()
		snap["schedule_active"] = sched.Active(now)
		next, willBeActive := sched.NextTransition(now)
		if !next.IsZero() {
			snap["schedule_next_transition"] = next
			snap["schedule_next_active"] = willBeActive
		}
	}

	return snap
}

// activeSegmentInfo holds a snapshot of the currently-open segment for API use.
type activeSegmentInfo struct {
	Label        string    `json:"label"`
	ChannelID    string    `json:"channel_id,omitempty"` // stable channel UUID
	FreqHz       int       `json:"freq_hz"`
	AudioMode    string    `json:"audio_mode"`
	SessionID    string    `json:"session_id"`
	SegmentIndex int       `json:"segment_index"`
	StartedAt    time.Time `json:"started_at"`
	BytesWritten int64     `json:"bytes_written"`
	DurationSec  float64   `json:"duration_sec"`
	SampleRate   int       `json:"sample_rate"`
	Channels     int       `json:"channels"`
	SNR          SNRStats  `json:"snr"`
	JsonlPath    string    `json:"-"` // not exposed in JSON
	Path         string    `json:"-"` // not exposed in JSON
}

// liveSnapshot returns info about the currently-open segment, or nil if not recording.
func (c *recChannel) liveSnapshot() *activeSegmentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil {
		return nil
	}
	dur := 0.0
	if c.current.sampleRate > 0 && c.current.channels > 0 {
		dur = float64(c.current.bytesWritten) / float64(c.current.sampleRate*c.current.channels*2)
	}
	jsonlPath := ""
	if c.current.jsonlFile != nil {
		jsonlPath = c.current.jsonlFile.Name()
	}
	return &activeSegmentInfo{
		Label:        c.label,
		ChannelID:    c.channelID,
		FreqHz:       c.inst.freqHz,
		AudioMode:    c.inst.audioMode,
		SessionID:    c.sessionID,
		SegmentIndex: c.segmentIndex,
		StartedAt:    c.current.startedAt,
		BytesWritten: c.current.bytesWritten,
		DurationSec:  dur,
		SampleRate:   c.current.sampleRate,
		Channels:     c.current.channels,
		SNR:          c.inst.snrAccum.peekLatest(2),
		JsonlPath:    jsonlPath,
		Path:         c.current.path,
	}
}

// ---------------------------------------------------------------------------
// WAV header helpers
// ---------------------------------------------------------------------------

// writeWAVHeader writes a standard 44-byte PCM WAV header.
// dataBytes is the number of PCM data bytes (0 for a placeholder).
func writeWAVHeader(f *os.File, sampleRate, channels, dataBytes int) error {
	bitsPerSample := 16
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	chunkSize := 36 + dataBytes

	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(chunkSize))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16) // PCM chunk size
	binary.LittleEndian.PutUint16(hdr[20:], 1)  // PCM format
	binary.LittleEndian.PutUint16(hdr[22:], uint16(channels))
	binary.LittleEndian.PutUint32(hdr[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(hdr[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:], uint16(bitsPerSample))
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(dataBytes))

	_, err := f.Write(hdr)
	return err
}
