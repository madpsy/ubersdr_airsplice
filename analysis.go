// analysis.go — optional post-recording analysis of closed segments.
//
// Two features, both off unless the operator turns them on:
//
//   - Classification (CLASSIFY): is a segment voice, music, voice+music or
//     noise? Ported from NightDX (github.com/marcozennaro/nightdx, MIT,
//     Copyright (c) 2026 Marco Zennaro), whose recordings it was tuned on.
//   - Transcription (TRANSCRIBE): Whisper, through UberSDR's whisper-helper
//     (clients/electron/whisper in the server tree) run as a child process.
//
// Each needs two switches. The global one (env var / flag) decides whether the
// host does the work at all: while it is off nothing is loaded, downloaded or
// started. The per-channel one (AnalysisConfig in channels.json, set in the
// web UI) decides which channels take part, so turning a feature on for the
// host does not start transcribing a channel that records around the clock.
// The model is chosen once for the host: one model loaded, and results that
// mean the same thing on every channel.
//
// Work happens after a segment closes, in the background, and never holds up
// recording. Two bounded queues: classification is cheap and runs first;
// transcription, which runs at about real time, takes only the segments that
// need it. A full queue drops its oldest job, which is marked skipped.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AnalysisConfig is a channel's opt-in, persisted in channels.json.
type AnalysisConfig struct {
	Classify   bool   `json:"classify"`
	Transcribe bool   `json:"transcribe"`
	Language   string `json:"language,omitempty"` // Whisper language code; "" or "auto" = detect
	// VoiceOnly transcribes only segments the classifier calls voice or
	// voice+music. It applies only while classification also runs for the
	// channel; otherwise every segment is transcribed.
	VoiceOnly bool `json:"voice_only"`
}

// analysisSettings are the host-wide switches, from flags and env vars.
type analysisSettings struct {
	Classify       bool
	ClassifyModel  string
	Transcribe     bool
	WhisperHelper  string
	WhisperModel   string
	WhisperThreads int
	WhisperSpeed   float64 // audio fed to the helper at this multiple of real time
	ModelDir       string
}

// classifyModels are the CLASSIFY_MODEL values this build implements.
var classifyModels = []string{"dsp"}

func (s *analysisSettings) validate() error {
	if s.Classify {
		ok := false
		for _, m := range classifyModels {
			if s.ClassifyModel == m {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("CLASSIFY_MODEL %q is not available in this build (available: %s)",
				s.ClassifyModel, strings.Join(classifyModels, ", "))
		}
	}
	if s.Transcribe {
		if _, ok := whisperModels[s.WhisperModel]; !ok {
			return fmt.Errorf("WHISPER_MODEL %q is not known (available: %s)", s.WhisperModel, strings.Join(whisperModelNames(), ", "))
		}
		if s.WhisperThreads < 1 {
			return fmt.Errorf("WHISPER_THREADS must be at least 1")
		}
		if s.WhisperSpeed < 0.5 || s.WhisperSpeed > 8 {
			return fmt.Errorf("WHISPER_SPEED must be between 0.5 and 8")
		}
	}
	return nil
}

// Result statuses.
const (
	analysisPending = "pending"
	analysisDone    = "done"
	analysisSkipped = "skipped"
	analysisFailed  = "failed"
)

// segmentAnalysis is what a segment's sidecar carries under "analysis".
type segmentAnalysis struct {
	Classification *classificationResult `json:"classification,omitempty"`
	Transcript     *transcriptResult     `json:"transcript,omitempty"`
}

type classificationResult struct {
	Status     string    `json:"status"`
	Label      string    `json:"label,omitempty"` // voice | music | voice+music | noise
	VoicePct   float64   `json:"voice_pct"`
	MusicPct   float64   `json:"music_pct"`
	NoisePct   float64   `json:"noise_pct"`
	Backend    string    `json:"backend,omitempty"` // the classifier that actually ran, e.g. "dsp", "dsp+snr"
	Confidence float64   `json:"confidence,omitempty"`
	Windows    []string  `json:"windows,omitempty"` // per-window verdicts, in order
	BandHz     []int     `json:"band_hz,omitempty"` // audio band measured, Hz
	Reason     string    `json:"reason,omitempty"`  // why skipped or failed
	At         time.Time `json:"at,omitempty"`
}

type transcriptResult struct {
	Status   string    `json:"status"`
	Language string    `json:"language,omitempty"` // the language asked for ("auto" = detected by Whisper)
	Model    string    `json:"model,omitempty"`
	Text     string    `json:"text,omitempty"`
	Lines    []string  `json:"lines,omitempty"`   // one per utterance
	Partial  bool      `json:"partial,omitempty"` // the helper fell behind and skipped some audio
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at,omitempty"`
}

// minAnalysisSec: shorter segments are skipped. The classifier needs a second
// of audio for a window, and a VOX blip is not worth a Whisper session.
const minAnalysisSec = 2.0

// Below this average passband SNR a segment is noise whatever the classifier
// hears in the hiss: an empty channel reads ~0 dB on the v4 scale with p95
// <= 3.8 (README, "SNR scale"). NightDX applies the same rule to its sideband
// measurement.
const noiseSNRdB = 3.0

const (
	classifyQueueMax   = 256
	transcribeQueueMax = 32
)

type analysisJob struct {
	recID      string
	wavPath    string
	cfg        AnalysisConfig
	mode       string // the channel's audio mode and filter bandwidth, which
	bwHz       int    // set the classifier's band (analysisBand)
	durSec     float64
	snrAvg     float32
	snrCount   int
	classify   bool
	transcribe bool
}

// jobQueue is a bounded FIFO that drops its oldest entry when full.
type jobQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []analysisJob
	max    int
	closed bool
	onDrop func(analysisJob)
}

func newJobQueue(max int, onDrop func(analysisJob)) *jobQueue {
	q := &jobQueue{max: max, onDrop: onDrop}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *jobQueue) push(j analysisJob) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	var dropped []analysisJob
	for len(q.items) >= q.max {
		dropped = append(dropped, q.items[0])
		q.items = q.items[1:]
	}
	q.items = append(q.items, j)
	q.cond.Signal()
	q.mu.Unlock()
	for _, d := range dropped {
		if q.onDrop != nil {
			q.onDrop(d)
		}
	}
}

// pop blocks until a job is available; ok is false once the queue is closed.
func (q *jobQueue) pop() (analysisJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return analysisJob{}, false
	}
	j := q.items[0]
	q.items = q.items[1:]
	return j, true
}

func (q *jobQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *jobQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// analyzer owns the queues and workers.
type analyzer struct {
	settings analysisSettings
	store    *recordingStore
	hub      *sseHub

	classifyQ   *jobQueue
	transcribeQ *jobQueue
	whisper     *whisperRunner // nil unless TRANSCRIBE is on

	// channelLookup returns a channel's current opt-in and filter bandwidth,
	// for segments re-queued after a restart (a segment does not record them).
	channelLookup func(channelID string) (AnalysisConfig, int, bool)
}

func newAnalyzer(s analysisSettings, store *recordingStore, hub *sseHub) *analyzer {
	a := &analyzer{settings: s, store: store, hub: hub}
	a.classifyQ = newJobQueue(classifyQueueMax, func(j analysisJob) {
		a.markSkipped(j, "analysis queue full")
	})
	a.transcribeQ = newJobQueue(transcribeQueueMax, func(j analysisJob) {
		a.setTranscript(j.recID, &transcriptResult{Status: analysisSkipped, Reason: "transcription queue full", At: time.Now().UTC()})
	})
	if s.Transcribe {
		a.whisper = newWhisperRunner(s)
	}
	return a
}

// enabled reports whether either feature is on for the host.
func (a *analyzer) enabled() bool {
	return a != nil && (a.settings.Classify || a.settings.Transcribe)
}

// start launches the workers. Work a restart left pending is re-queued by
// requeuePending, once the channels are loaded.
func (a *analyzer) start(ctx context.Context) {
	if !a.enabled() {
		return
	}
	if a.settings.Classify {
		log.Printf("[analysis] classification enabled (model %s)", a.settings.ClassifyModel)
	}
	if a.whisper != nil {
		log.Printf("[analysis] transcription enabled (model %s, %d threads, %.1fx real time)",
			a.settings.WhisperModel, a.settings.WhisperThreads, a.settings.WhisperSpeed)
		go a.whisper.prepare(ctx)
	}
	go a.classifyWorker()
	go a.transcribeWorker(ctx)
	go func() {
		<-ctx.Done()
		a.classifyQ.close()
		a.transcribeQ.close()
		if a.whisper != nil {
			a.whisper.shutdown()
		}
	}()
}

// enqueue is called when a segment closes. cfg is the channel's opt-in at that
// moment; later changes do not affect a segment already queued.
func (a *analyzer) enqueue(rec *recordingRecord, cfg AnalysisConfig, bwHz int) {
	if !a.enabled() {
		return
	}
	j := analysisJob{
		recID:      rec.ID,
		wavPath:    filepath.Join(a.store.outputDir, rec.Filename),
		cfg:        cfg,
		mode:       rec.AudioMode,
		bwHz:       bwHz,
		durSec:     rec.DurationSec,
		snrAvg:     rec.SNR.AvgDB,
		snrCount:   rec.SNR.Count,
		classify:   a.settings.Classify && cfg.Classify,
		transcribe: a.settings.Transcribe && cfg.Transcribe,
	}
	if !j.classify && !j.transcribe {
		return
	}
	now := time.Now().UTC()
	if j.durSec < minAnalysisSec {
		a.markSkipped(j, fmt.Sprintf("shorter than %.0f s", minAnalysisSec))
		return
	}
	a.update(j.recID, func(an *segmentAnalysis) {
		if j.classify {
			an.Classification = &classificationResult{Status: analysisPending, At: now}
		}
		if j.transcribe {
			an.Transcript = &transcriptResult{Status: analysisPending, Model: a.settings.WhisperModel, Language: langOrAuto(cfg.Language), At: now}
		}
	})
	a.classifyQ.push(j)
}

// requeuePending picks up segments a restart left pending. The segment's own
// record says which features were pending; the language, voice-only choice and
// bandwidth come from its channel as it is configured now.
func (a *analyzer) requeuePending() {
	if !a.enabled() {
		return
	}
	a.store.mu.RLock()
	var jobs []analysisJob
	for _, r := range a.store.records {
		an := r.Analysis
		if an == nil {
			continue
		}
		c := an.Classification != nil && an.Classification.Status == analysisPending
		t := an.Transcript != nil && an.Transcript.Status == analysisPending
		if !c && !t {
			continue
		}
		cfg := AnalysisConfig{Classify: c, Transcribe: t}
		if an.Transcript != nil && an.Transcript.Language != "auto" {
			cfg.Language = an.Transcript.Language
		}
		bw := 0
		if a.channelLookup != nil {
			if cur, chBW, ok := a.channelLookup(r.ChannelID); ok {
				cfg.VoiceOnly, cfg.Language, bw = cur.VoiceOnly, cur.Language, chBW
			}
		}
		jobs = append(jobs, analysisJob{
			recID: r.ID, wavPath: filepath.Join(a.store.outputDir, r.Filename), cfg: cfg, mode: r.AudioMode, bwHz: bw,
			durSec: r.DurationSec, snrAvg: r.SNR.AvgDB, snrCount: r.SNR.Count,
			classify: c && a.settings.Classify, transcribe: t && a.settings.Transcribe,
		})
	}
	a.store.mu.RUnlock()
	for _, j := range jobs {
		if !j.classify && !j.transcribe {
			// The feature was turned off since: say so rather than leave it pending.
			a.markSkipped(j, "feature disabled")
			continue
		}
		a.classifyQ.push(j)
	}
	if len(jobs) > 0 {
		log.Printf("[analysis] re-queued %d segment(s) left pending", len(jobs))
	}
}

func (a *analyzer) classifyWorker() {
	for {
		j, ok := a.classifyQ.pop()
		if !ok {
			return
		}
		var label string
		if j.classify {
			res := a.runClassify(j)
			label = res.Label
			a.update(j.recID, func(an *segmentAnalysis) { an.Classification = res })
		}
		if !j.transcribe {
			continue
		}
		if j.classify && j.cfg.VoiceOnly && label != "voice" && label != "voice+music" {
			reason := "no voice detected"
			if label == "" {
				reason = "classification failed"
			}
			a.setTranscript(j.recID, &transcriptResult{Status: analysisSkipped, Reason: reason, At: time.Now().UTC()})
			continue
		}
		a.transcribeQ.push(j)
	}
}

func (a *analyzer) runClassify(j analysisJob) *classificationResult {
	now := time.Now().UTC()
	audio, rate, err := readWAVMono(j.wavPath)
	if err != nil {
		return &classificationResult{Status: analysisFailed, Reason: err.Error(), At: now}
	}
	audio = resample(audio, rate, analysisRate)
	lo, hi := analysisBand(j.mode, j.bwHz)
	res := classifyDSP(audio, analysisRate, dspWindowSec, lo, hi)
	res.At = time.Now().UTC()
	if j.snrCount > 0 && j.snrAvg < noiseSNRdB && res.Label != "noise" {
		res.Label = "noise"
		res.VoicePct, res.MusicPct, res.NoisePct = 0, 0, 100
		res.Backend += "+snr"
		for i := range res.Windows {
			res.Windows[i] = "noise"
		}
	}
	return res
}

func (a *analyzer) transcribeWorker(ctx context.Context) {
	for {
		j, ok := a.transcribeQ.pop()
		if !ok {
			return
		}
		res := a.whisper.transcribe(ctx, j)
		a.setTranscript(j.recID, res)
	}
}

func (a *analyzer) markSkipped(j analysisJob, reason string) {
	now := time.Now().UTC()
	a.update(j.recID, func(an *segmentAnalysis) {
		if j.classify || (an.Classification != nil && an.Classification.Status == analysisPending) {
			an.Classification = &classificationResult{Status: analysisSkipped, Reason: reason, At: now}
		}
		if j.transcribe || (an.Transcript != nil && an.Transcript.Status == analysisPending) {
			an.Transcript = &transcriptResult{Status: analysisSkipped, Reason: reason, At: now}
		}
	})
}

func (a *analyzer) setTranscript(recID string, t *transcriptResult) {
	if t.Model == "" {
		t.Model = a.settings.WhisperModel
	}
	a.update(recID, func(an *segmentAnalysis) {
		if t.Language == "" && an.Transcript != nil {
			t.Language = an.Transcript.Language
		}
		an.Transcript = t
	})
}

// update applies fn to the segment's analysis, persists it and tells the UI.
// A segment deleted in the meantime is ignored.
func (a *analyzer) update(recID string, fn func(*segmentAnalysis)) {
	rec, err := a.store.updateAnalysis(recID, fn)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("[analysis] %s: %v", shortID(recID), err)
		}
		return
	}
	a.hub.broadcast(sseEvent{
		Event: "recording_analysed",
		Data: map[string]interface{}{
			"id":         rec.ID,
			"channel_id": rec.ChannelID,
			"session_id": rec.SessionID,
			"analysis":   rec.Analysis,
		},
	})
}

// statusSnapshot is GET /api/analysis: what the host offers, for the UI.
func (a *analyzer) statusSnapshot() map[string]interface{} {
	s := a.settings
	classify := map[string]interface{}{
		"enabled": s.Classify,
		"model":   s.ClassifyModel,
		"models":  classifyModels,
	}
	transcribe := map[string]interface{}{
		"enabled": s.Transcribe,
		"model":   s.WhisperModel,
		"models":  whisperModelNames(),
		"state":   "disabled",
	}
	if a != nil {
		classify["queued"] = a.classifyQ.len()
		transcribe["queued"] = a.transcribeQ.len()
		if a.whisper != nil {
			for k, v := range a.whisper.status() {
				transcribe[k] = v
			}
		}
	}
	return map[string]interface{}{"classify": classify, "transcribe": transcribe}
}

func langOrAuto(l string) string {
	if l == "" {
		return "auto"
	}
	return l
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
