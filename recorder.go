// recorder.go — per-channel audio recorder: wires instance → WAV file writer
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
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

	mu      sync.Mutex
	current *activeRecording // nil when not recording
}

// activeRecording holds state for the currently-open WAV file.
type activeRecording struct {
	id         string
	file       *os.File
	path       string
	startedAt  time.Time
	sampleRate int
	channels   int
	bytesWritten int64 // PCM bytes written (excludes header)
}

func newRecChannel(inst *instance, store *recordingStore, hub *sseHub, segmentSecs int) *recChannel {
	return &recChannel{
		inst:        inst,
		store:       store,
		hub:         hub,
		segmentSecs: segmentSecs,
		label:       inst.label,
	}
}

// run starts the channel and blocks until ctx is cancelled.
func (c *recChannel) run(ctx context.Context) {
	go c.inst.start(ctx)

	// Wait for first audio packet to determine sample rate.
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

	c.inst.streamMu.RLock()
	sampleRate := c.inst.streamSampleRate
	numChannels := c.inst.streamChannels
	c.inst.streamMu.RUnlock()

	if sampleRate == 0 {
		sampleRate = 8000
	}
	if numChannels == 0 {
		numChannels = 1
	}
	// Audio is always delivered as mono S16LE after downmix in ubersdr.go
	numChannels = 1

	log.Printf("[%s] starting recorder: %d Hz, %d ch, segment=%ds", c.label, sampleRate, numChannels, c.segmentSecs)

	// Open the first segment.
	if err := c.openSegment(sampleRate, numChannels); err != nil {
		log.Printf("[%s] open segment: %v", c.label, err)
		return
	}

	// Segment rotation ticker (disabled when segmentSecs == 0).
	var rotateCh <-chan time.Time
	if c.segmentSecs > 0 {
		ticker := time.NewTicker(time.Duration(c.segmentSecs) * time.Second)
		defer ticker.Stop()
		rotateCh = ticker.C
	}

	// Feed the first chunk.
	c.write(firstChunk)

	for {
		select {
		case <-ctx.Done():
			c.closeSegment()
			return
		case <-rotateCh:
			if err := c.rotateSegment(sampleRate, numChannels); err != nil {
				log.Printf("[%s] rotate segment: %v", c.label, err)
			}
		case chunk, ok := <-c.inst.AudioCh:
			if !ok {
				c.closeSegment()
				return
			}
			c.write(chunk)
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

// openSegment creates a new WAV file and writes a placeholder header.
func (c *recChannel) openSegment(sampleRate, channels int) error {
	if err := os.MkdirAll(c.store.outputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	id := uuid.New().String()
	ts := time.Now().UTC()
	base := fmt.Sprintf("%s_%s_%s", ts.Format("20060102_150405"), c.label, id[:8])
	fname := base + ".wav"
	path := filepath.Join(c.store.outputDir, fname)

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	// Write a placeholder WAV header (will be finalised on close).
	if err := writeWAVHeader(f, sampleRate, channels, 0); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write header: %w", err)
	}

	c.mu.Lock()
	c.current = &activeRecording{
		id:         id,
		file:       f,
		path:       path,
		startedAt:  ts,
		sampleRate: sampleRate,
		channels:   channels,
	}
	c.mu.Unlock()

	log.Printf("[%s] opened segment %s", c.label, fname)

	c.hub.broadcast(sseEvent{
		Event: "recording_started",
		Data: map[string]interface{}{
			"label":   c.label,
			"freq_hz": c.inst.freqHz,
			"id":      id,
		},
	})

	return nil
}

// closeSegment finalises the WAV header and saves the record.
func (c *recChannel) closeSegment() {
	c.mu.Lock()
	rec := c.current
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

	fname := filepath.Base(rec.path)
	durationSecs := 0.0
	if rec.sampleRate > 0 && rec.channels > 0 {
		durationSecs = float64(rec.bytesWritten) / float64(rec.sampleRate*rec.channels*2)
	}

	snr := c.inst.DrainSNR()

	r := &recordingRecord{
		ID:          rec.id,
		Label:       c.label,
		FreqHz:      c.inst.freqHz,
		AudioMode:   c.inst.audioMode,
		StartedAt:   rec.startedAt,
		SavedAt:     time.Now().UTC(),
		DurationSec: durationSecs,
		SampleRate:  rec.sampleRate,
		Channels:    rec.channels,
		Filename:    fname,
		SNR:         snr,
	}

	if err := c.store.add(r); err != nil {
		log.Printf("[%s] store.add: %v", c.label, err)
	}

	log.Printf("[%s] closed segment %s (%.1fs, %.1f kB)", c.label, fname, durationSecs, float64(rec.bytesWritten)/1024)

	c.hub.broadcast(sseEvent{
		Event: "recording_saved",
		Data:  r,
	})
}

// rotateSegment closes the current segment and opens a new one.
func (c *recChannel) rotateSegment(sampleRate, channels int) error {
	c.closeSegment()
	return c.openSegment(sampleRate, channels)
}

// statusSnapshot returns a JSON-friendly status map for this channel.
func (c *recChannel) statusSnapshot() map[string]interface{} {
	snap := c.inst.statusSnapshot()
	c.mu.Lock()
	if c.current != nil {
		snap["recording"] = true
		snap["segment_started_at"] = c.current.startedAt
		snap["segment_bytes"] = c.current.bytesWritten
	} else {
		snap["recording"] = false
	}
	c.mu.Unlock()
	return snap
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
