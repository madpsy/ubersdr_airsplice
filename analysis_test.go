package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── audio helpers ────────────────────────────────────────────────────────────

func writeTestWAV(t *testing.T, path string, rate int, x []float32, finalise bool) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	size := len(x) * 2
	if !finalise {
		size = 0 // as an orphaned segment's placeholder header
	}
	if err := writeWAVHeader(f, rate, 1, size); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2*len(x))
	for i, v := range x {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(floatToInt16(v)))
	}
	if _, err := f.Write(buf); err != nil {
		t.Fatal(err)
	}
}

func sine(rate int, hz, amp, secs float64) []float32 {
	n := int(float64(rate) * secs)
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(amp * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
	}
	return x
}

func TestReadWAVMonoRoundTrip(t *testing.T) {
	dir := t.TempDir()
	x := sine(12000, 440, 0.5, 1)
	for _, finalise := range []bool{true, false} {
		p := filepath.Join(dir, "a.wav")
		writeTestWAV(t, p, 12000, x, finalise)
		y, rate, err := readWAVMono(p)
		if err != nil {
			t.Fatalf("finalise=%v: %v", finalise, err)
		}
		if rate != 12000 || len(y) != len(x) {
			t.Fatalf("finalise=%v: rate %d len %d, want 12000 %d", finalise, rate, len(y), len(x))
		}
		for i := range x {
			if d := math.Abs(float64(x[i] - y[i])); d > 1.0/16384 {
				t.Fatalf("sample %d: %v vs %v", i, x[i], y[i])
			}
		}
	}
}

func TestReadWAVMonoRejectsNonWAV(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.wav")
	_ = os.WriteFile(p, []byte("not a wav file at all"), 0o644)
	if _, _, err := readWAVMono(p); err == nil {
		t.Fatal("garbage accepted as WAV")
	}
}

// rmsAt measures a sine's amplitude at hz by correlation (ignoring the edges).
func rmsAt(x []float32, rate int, hz float64) float64 {
	var re, im float64
	lo, hi := len(x)/10, len(x)*9/10
	for i := lo; i < hi; i++ {
		a := 2 * math.Pi * hz * float64(i) / float64(rate)
		re += float64(x[i]) * math.Cos(a)
		im += float64(x[i]) * math.Sin(a)
	}
	n := float64(hi - lo)
	return 2 * math.Hypot(re, im) / n
}

func TestResampleKeepsToneAndLevel(t *testing.T) {
	for _, from := range []int{12000, 24000, 48000} {
		x := sine(from, 1000, 0.5, 2)
		y := resample(x, from, analysisRate)
		want := len(x) * analysisRate / from
		if len(y) != want {
			t.Fatalf("%d Hz: %d samples out, want %d", from, len(y), want)
		}
		if a := rmsAt(y, analysisRate, 1000); math.Abs(a-0.5) > 0.01 {
			t.Errorf("%d Hz: 1 kHz tone at %.3f after resampling, want 0.5", from, a)
		}
	}
}

func TestResampleRemovesAliases(t *testing.T) {
	// 10 kHz is above the 8 kHz Nyquist of 16 kHz: it must be filtered out,
	// not folded down to 6 kHz.
	y := resample(sine(24000, 10000, 0.5, 2), 24000, analysisRate)
	if a := rmsAt(y, analysisRate, 6000); a > 0.005 {
		t.Errorf("10 kHz folded to 6 kHz at %.4f", a)
	}
}

// ── classifier ───────────────────────────────────────────────────────────────

func whiteNoise(n int, amp float64, seed int64) []float32 {
	r := rand.New(rand.NewSource(seed))
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(amp * r.NormFloat64())
	}
	return x
}

// chords: sustained harmonic notes, changing every second — steady spectra.
func chords(rate int, secs float64) []float32 {
	notes := []float64{220, 261.6, 329.6, 392}
	n := int(float64(rate) * secs)
	x := make([]float32, n)
	for i := range x {
		tt := float64(i) / float64(rate)
		f0 := notes[int(tt)%len(notes)]
		var v float64
		for h := 1; h <= 6; h++ {
			v += math.Sin(2*math.Pi*f0*float64(h)*tt) / float64(h)
		}
		x[i] = float32(0.2 * v)
	}
	return x
}

// babble: a voiced buzz whose loudness rises and falls ~4 times a second and
// whose pitch and formant move, as syllables do.
func babble(rate int, secs float64) []float32 {
	n := int(float64(rate) * secs)
	x := make([]float32, n)
	r := rand.New(rand.NewSource(3))
	phase := 0.0
	syl := 0.25
	next, f0, formant := 0.0, 120.0, 700.0
	for i := range x {
		tt := float64(i) / float64(rate)
		if tt >= next {
			syl = 0.18 + 0.14*r.Float64()
			next = tt + syl
			f0 = 100 + 80*r.Float64()
			formant = 400 + 1800*r.Float64()
		}
		pos := 1 - (next-tt)/syl // 0..1 through the syllable
		env := math.Pow(math.Sin(math.Pi*pos), 2)
		phase += 2 * math.Pi * f0 / float64(rate)
		var v float64
		for h := 1; h <= 25; h++ {
			fh := f0 * float64(h)
			g := math.Exp(-math.Pow((fh-formant)/400, 2))
			v += g * math.Sin(phase*float64(h))
		}
		x[i] = float32(0.3 * env * v)
	}
	return x
}

func TestClassifyDSPNoise(t *testing.T) {
	r := classifyDSP(whiteNoise(analysisRate*20, 0.1, 1), analysisRate, dspWindowSec, 250, 3500)
	if r.Label != "noise" {
		t.Errorf("white noise classified as %s (%v)", r.Label, r.Windows)
	}
}

func TestClassifyDSPMusic(t *testing.T) {
	r := classifyDSP(chords(analysisRate, 20), analysisRate, dspWindowSec, 250, 3500)
	if r.Label != "music" {
		t.Errorf("sustained chords classified as %s (%v)", r.Label, r.Windows)
	}
}

// ssbNoise is white noise through an ideal 300-2700 Hz filter: an empty SSB
// channel, as airsplice records one.
func ssbNoise(seed int64) []float32 {
	const n = 1 << 18 // 16.4 s at 16 kHz
	r := rand.New(rand.NewSource(seed))
	x := make([]complex128, n)
	for i := range x {
		x[i] = complex(r.NormFloat64(), 0)
	}
	fftRadix2(x)
	for k := range x {
		f := float64(k) * analysisRate / n
		if k > n/2 {
			f = float64(n-k) * analysisRate / n
		}
		if f < 300 || f > 2700 {
			x[k] = 0
		}
	}
	for i := range x { // inverse FFT by conjugation
		x[i] = complex(real(x[i]), -imag(x[i]))
	}
	fftRadix2(x)
	out := make([]float32, n)
	for i := range x {
		out[i] = float32(0.1 * real(x[i]) / n * 4)
	}
	return out
}

// The defect analysisBand fixes: with NightDX's 250-3500 Hz band the SSB
// filter skirt sits inside the measurement and its fixed shape makes an empty
// channel look as steady as held notes.
func TestClassifyDSPEmptySSBChannelIsNoise(t *testing.T) {
	x := ssbNoise(7)
	lo, hi := analysisBand("usb", 2700)
	if lo != 300 || hi < 2400 || hi > 2450 {
		t.Fatalf("usb 2700 Hz band %.0f-%.0f", lo, hi)
	}
	if r := classifyDSP(x, analysisRate, dspWindowSec, lo, hi); r.Label != "noise" {
		t.Errorf("empty SSB channel classified as %s (%v) with the passband cap", r.Label, r.Windows)
	}
	old := classifyDSP(x, analysisRate, dspWindowSec, 250, 3500)
	t.Logf("with NightDX's 250-3500 Hz band the same channel reads %s (%v)", old.Label, old.Windows)
}

func TestAnalysisBand(t *testing.T) {
	cases := []struct {
		mode   string
		bw     int
		lo, hi float64
	}{
		{"usb", 0, 300, 2430}, {"lsb", 2400, 300, 2160}, {"am", 0, 250, 2250},
		{"am", 9000, 250, 3500}, {"fm", 0, 250, 3500}, {"cw", 500, 250, 3500},
		{"usb", 400, 300, 800}, // narrower than usable: a 500 Hz band is kept
	}
	for _, c := range cases {
		lo, hi := analysisBand(c.mode, c.bw)
		if lo != c.lo || math.Abs(hi-c.hi) > 0.5 {
			t.Errorf("%s %d Hz: %.0f-%.0f, want %.0f-%.0f", c.mode, c.bw, lo, hi, c.lo, c.hi)
		}
	}
}

func TestClassifyDSPTooShort(t *testing.T) {
	r := classifyDSP(whiteNoise(analysisRate/2, 0.1, 2), analysisRate, dspWindowSec, 250, 3500)
	if r.Label != "noise" || len(r.Windows) != 0 || r.Reason == "" {
		t.Errorf("half a second: %+v", r)
	}
}

func TestFoldWindows(t *testing.T) {
	cases := []struct {
		w    []string
		want string
	}{
		{[]string{"voice", "voice", "noise"}, "voice"},
		{[]string{"music", "music", "voice"}, "voice+music"},
		{[]string{"music", "music", "music", "music", "voice"}, "music"},
		{[]string{"noise", "noise", "noise", "voice", "music"}, "noise"},
		{[]string{"voice", "music"}, "voice+music"},
	}
	for _, c := range cases {
		if got := foldWindows(c.w, "dsp", 0.5).Label; got != c.want {
			t.Errorf("%v: %s, want %s", c.w, got, c.want)
		}
	}
}

// ── queue, config, store ─────────────────────────────────────────────────────

func TestJobQueueDropsOldest(t *testing.T) {
	var dropped []string
	q := newJobQueue(2, func(j analysisJob) { dropped = append(dropped, j.recID) })
	for _, id := range []string{"a", "b", "c"} {
		q.push(analysisJob{recID: id})
	}
	if len(dropped) != 1 || dropped[0] != "a" {
		t.Fatalf("dropped %v, want [a]", dropped)
	}
	j, _ := q.pop()
	if j.recID != "b" {
		t.Fatalf("popped %s, want b", j.recID)
	}
	q.close()
	if _, ok := q.pop(); !ok {
		t.Fatal("queued job lost on close")
	}
	if _, ok := q.pop(); ok {
		t.Fatal("pop after close and drain returned a job")
	}
}

func TestValidateAnalysisConfig(t *testing.T) {
	for _, l := range []string{"", "auto", "en", "yue", "haw"} {
		if e := validateAnalysisConfig(AnalysisConfig{Language: l}); e != "" {
			t.Errorf("%q rejected: %s", l, e)
		}
	}
	for _, l := range []string{"e", "EN", "en-GB", "../x", "englishlanguage"} {
		if e := validateAnalysisConfig(AnalysisConfig{Language: l}); e == "" {
			t.Errorf("%q accepted", l)
		}
	}
}

func TestAnalysisSettingsValidate(t *testing.T) {
	ok := analysisSettings{Classify: true, ClassifyModel: "dsp", ClassifyThreads: 1, Transcribe: true, WhisperModel: "base", WhisperThreads: 2, WhisperSpeed: 1}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"mn05", "mn10", "mn20"} {
		good := ok
		good.ClassifyModel = m
		if err := good.validate(); err != nil {
			t.Errorf("%s rejected: %v", m, err)
		}
	}
	bad := ok
	bad.ClassifyModel = "mn40"
	if err := bad.validate(); err == nil {
		t.Error("an unimplemented classifier was accepted")
	}
	bad = ok
	bad.ClassifyThreads = 0
	if err := bad.validate(); err == nil {
		t.Error("zero classifier threads accepted")
	}
	bad = ok
	bad.WhisperModel = "large"
	if err := bad.validate(); err == nil {
		t.Error("an unknown Whisper model was accepted")
	}
	// Off means off: nothing about a disabled feature is checked.
	off := analysisSettings{ClassifyModel: "nonsense", WhisperModel: "nonsense"}
	if err := off.validate(); err != nil {
		t.Errorf("disabled features validated: %v", err)
	}
}

func TestChannelConfigAnalysisRoundTrip(t *testing.T) {
	in := channelConfig{ID: "x", FreqHz: 6000000, Mode: "am",
		Analysis: AnalysisConfig{Classify: true, Transcribe: true, Language: "de", VoiceOnly: true}}
	b, _ := json.Marshal(in)
	var out channelConfig
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Analysis != in.Analysis {
		t.Fatalf("%+v != %+v", out.Analysis, in.Analysis)
	}
	// A channels.json from before this feature loads with everything off.
	var old channelConfig
	_ = json.Unmarshal([]byte(`{"id":"y","freq_hz":7000000,"mode":"usb"}`), &old)
	if old.Analysis != (AnalysisConfig{}) {
		t.Fatalf("old config: %+v", old.Analysis)
	}
}

func newTestStore(t *testing.T) *recordingStore {
	t.Helper()
	return newRecordingStore(t.TempDir(), newSSEHub())
}

func addTestSegment(t *testing.T, s *recordingStore, x []float32, rate int, snr float32) *recordingRecord {
	t.Helper()
	id := "seg-" + strings.ReplaceAll(t.Name(), "/", "_")
	writeTestWAV(t, filepath.Join(s.outputDir, id+".wav"), rate, x, true)
	r := &recordingRecord{ID: id, SessionID: "sess-1", Label: "test", FreqHz: 6000000, AudioMode: "am",
		StartedAt: time.Now().UTC(), SavedAt: time.Now().UTC(), DurationSec: float64(len(x)) / float64(rate),
		SampleRate: rate, Channels: 1, Filename: id + ".wav", SNR: SNRStats{Count: 100, AvgDB: snr}}
	if err := s.add(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUpdateAnalysisIsCopyOnWrite(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, sine(12000, 440, 0.1, 1), 12000, 20)
	got, err := s.updateAnalysis(r.ID, func(a *segmentAnalysis) {
		a.Classification = &classificationResult{Status: analysisDone, Label: "music"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Analysis != nil {
		t.Fatal("the original record was modified in place")
	}
	if got.Analysis.Classification.Label != "music" {
		t.Fatal("update not applied")
	}
	// The sidecar on disk carries it, so it survives a restart.
	b, _ := os.ReadFile(filepath.Join(s.outputDir, r.ID+".json"))
	var disk recordingRecord
	_ = json.Unmarshal(b, &disk)
	if disk.Analysis == nil || disk.Analysis.Classification.Label != "music" {
		t.Fatalf("sidecar: %s", b)
	}
	if _, err := s.updateAnalysis("missing", func(*segmentAnalysis) {}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing segment: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func analysisOf(s *recordingStore, id string) *segmentAnalysis {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r := s.byID[id]; r != nil {
		return r.Analysis
	}
	return nil
}

func TestAnalyzerClassifiesClosedSegment(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, chords(24000, 12), 24000, 25)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	az := newAnalyzer(analysisSettings{Classify: true, ClassifyModel: "dsp"}, s, s.hub)
	az.start(ctx)
	az.enqueue(r, AnalysisConfig{Classify: true}, 0)
	waitFor(t, "classification", func() bool {
		a := analysisOf(s, r.ID)
		return a != nil && a.Classification != nil && a.Classification.Status == analysisDone
	})
	if c := analysisOf(s, r.ID).Classification; c.Label != "music" || c.Backend != "dsp" {
		t.Fatalf("%+v", c)
	}
}

func TestAnalyzerLowSNRIsNoise(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, chords(24000, 12), 24000, 0.5) // an empty channel's SNR
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	az := newAnalyzer(analysisSettings{Classify: true, ClassifyModel: "dsp"}, s, s.hub)
	az.start(ctx)
	az.enqueue(r, AnalysisConfig{Classify: true}, 0)
	waitFor(t, "classification", func() bool {
		a := analysisOf(s, r.ID)
		return a != nil && a.Classification != nil && a.Classification.Status == analysisDone
	})
	if c := analysisOf(s, r.ID).Classification; c.Label != "noise" || c.Backend != "dsp+snr" {
		t.Fatalf("%+v", c)
	}
}

func TestAnalyzerOffDoesNothing(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, chords(24000, 4), 24000, 25)
	az := newAnalyzer(analysisSettings{ClassifyModel: "dsp"}, s, s.hub) // both features off
	az.start(context.Background())
	az.enqueue(r, AnalysisConfig{Classify: true, Transcribe: true}, 0) // the channel opted in; the host did not
	time.Sleep(100 * time.Millisecond)
	if a := analysisOf(s, r.ID); a != nil {
		t.Fatalf("analysis ran with the features off: %+v", a)
	}
	if az.whisper != nil {
		t.Fatal("whisper runner created with transcription off")
	}
}

func TestAnalyzerSkipsShortSegments(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, chords(24000, 1), 24000, 25)
	az := newAnalyzer(analysisSettings{Classify: true, ClassifyModel: "dsp"}, s, s.hub)
	az.enqueue(r, AnalysisConfig{Classify: true}, 0)
	if c := analysisOf(s, r.ID).Classification; c.Status != analysisSkipped {
		t.Fatalf("%+v", c)
	}
}

// ── transcription, against a fake whisper-helper ─────────────────────────────

// The test binary doubles as a fake whisper-helper when this is set. It
// speaks the real framing: on START it remembers the session; on the first
// silent AUDIO frame after sound it emits one final, so the closing silence
// airsplice appends is what triggers it, as with the real VAD.
const fakeHelperEnv = "AIRSPLICE_FAKE_WHISPER_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeHelperEnv); mode != "" {
		fakeHelper(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeHelper(mode string) {
	for _, a := range os.Args[1:] {
		switch a {
		case "--version":
			os.Stdout.WriteString("whisper-helper 1 (fake)\n")
			return
		case "--languages":
			os.Stdout.WriteString(`["en","de","fr"]` + "\n")
			return
		}
	}
	out := bufio.NewWriter(os.Stdout)
	say := func(v interface{}) {
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	say(map[string]string{"type": "ready"})
	in := bufio.NewReader(os.Stdin)
	var session int64
	var lang string
	heard, spoken := false, false
	for {
		var head [5]byte
		if _, err := io.ReadFull(in, head[:]); err != nil {
			return
		}
		body := make([]byte, binary.LittleEndian.Uint32(head[1:]))
		if _, err := io.ReadFull(in, body); err != nil {
			return
		}
		switch head[0] {
		case frameStart:
			var st struct {
				Lang    string `json:"lang"`
				Session int64  `json:"session"`
			}
			_ = json.Unmarshal(body, &st)
			session, lang, heard, spoken = st.Session, st.Lang, false, false
			if mode == "behind" {
				say(map[string]interface{}{"type": "error", "session": session, "code": "falling-behind", "message": "behind", "fatal": false})
			}
		case frameAudio:
			silent := true
			for i := 0; i+1 < len(body); i += 2 {
				if v := int16(binary.LittleEndian.Uint16(body[i:])); v > 300 || v < -300 {
					silent = false
					break
				}
			}
			if !silent {
				heard = true
			} else if heard && !spoken {
				spoken = true
				if mode == "crash" {
					os.Exit(3)
				}
				say(map[string]interface{}{"type": "result", "session": session, "text": "interim", "final": false})
				say(map[string]interface{}{"type": "result", "session": session, "text": "hello in " + lang, "final": true})
			}
		}
	}
}

func fakeWhisperSettings(t *testing.T, mode string) analysisSettings {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeHelperEnv, mode)
	old := whisperSettle
	whisperSettle = 300 * time.Millisecond
	t.Cleanup(func() { whisperSettle = old })
	return analysisSettings{Transcribe: true, WhisperHelper: exe, WhisperModel: "tiny", WhisperThreads: 1, WhisperSpeed: 8}
}

// readyRunner is a whisperRunner past prepare(), with model paths that the
// fake helper ignores, so no model is downloaded.
func readyRunner(t *testing.T, s analysisSettings) *whisperRunner {
	w := newWhisperRunner(s)
	w.helperPath, w.modelPath, w.vadPath = s.WhisperHelper, "model.bin", "vad.bin"
	w.state = "ready"
	close(w.readyCh)
	t.Cleanup(w.shutdown)
	return w
}

func TestWhisperTranscribesSegment(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, babble(12000, 2), 12000, 20)
	w := readyRunner(t, fakeWhisperSettings(t, "ok"))
	res := w.transcribe(context.Background(), analysisJob{recID: r.ID, wavPath: filepath.Join(s.outputDir, r.Filename), cfg: AnalysisConfig{Language: "de"}})
	if res.Status != analysisDone || res.Text != "hello in de" || res.Partial || res.Language != "de" {
		t.Fatalf("%+v", res)
	}
	// The helper stays up: a second segment reuses it, as a new session.
	pid := w.proc.Process.Pid
	res = w.transcribe(context.Background(), analysisJob{recID: r.ID, wavPath: filepath.Join(s.outputDir, r.Filename)})
	if res.Text != "hello in auto" || w.proc.Process.Pid != pid {
		t.Fatalf("second segment: %+v (pid %d -> %d)", res, pid, w.proc.Process.Pid)
	}
}

func TestWhisperFallingBehindIsPartial(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, babble(12000, 2), 12000, 20)
	w := readyRunner(t, fakeWhisperSettings(t, "behind"))
	res := w.transcribe(context.Background(), analysisJob{recID: r.ID, wavPath: filepath.Join(s.outputDir, r.Filename)})
	if res.Status != analysisDone || !res.Partial {
		t.Fatalf("%+v", res)
	}
}

func TestWhisperHelperCrashFailsAndRestarts(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, babble(12000, 2), 12000, 20)
	w := readyRunner(t, fakeWhisperSettings(t, "crash"))
	job := analysisJob{recID: r.ID, wavPath: filepath.Join(s.outputDir, r.Filename)}
	if res := w.transcribe(context.Background(), job); res.Status != analysisFailed {
		t.Fatalf("crash: %+v", res)
	}
	if w.proc != nil {
		t.Fatal("dead helper kept")
	}
	t.Setenv(fakeHelperEnv, "ok")
	if res := w.transcribe(context.Background(), job); res.Status != analysisDone {
		t.Fatalf("after restart: %+v", res)
	}
}

func TestWhisperUnavailableWithoutHelper(t *testing.T) {
	w := newWhisperRunner(analysisSettings{Transcribe: true, WhisperHelper: "/nonexistent/whisper-helper", WhisperModel: "tiny", ModelDir: t.TempDir()})
	w.prepare(context.Background())
	st := w.status()
	if st["state"] != "unavailable" || !strings.Contains(st["error"].(string), "WHISPER_HELPER") {
		t.Fatalf("%v", st)
	}
	res := w.transcribe(context.Background(), analysisJob{})
	if res.Status != analysisFailed {
		t.Fatalf("%+v", res)
	}
}

func TestAnalyzerVoiceOnlySkipsMusic(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, chords(24000, 12), 24000, 25)
	set := fakeWhisperSettings(t, "ok")
	set.Classify, set.ClassifyModel = true, "dsp"
	az := newAnalyzer(set, s, s.hub)
	az.whisper = readyRunner(t, set)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go az.classifyWorker()
	go az.transcribeWorker(ctx)
	az.enqueue(r, AnalysisConfig{Classify: true, Transcribe: true, VoiceOnly: true}, 0)
	waitFor(t, "transcript verdict", func() bool {
		a := analysisOf(s, r.ID)
		return a != nil && a.Transcript != nil && a.Transcript.Status != analysisPending
	})
	tr := analysisOf(s, r.ID).Transcript
	if tr.Status != analysisSkipped || tr.Reason != "no voice detected" {
		t.Fatalf("%+v", tr)
	}
}

func TestEnsureModelRejectsWrongFile(t *testing.T) {
	// A file already on disk with the wrong content is not trusted; with no
	// network reachable the download fails rather than accepting it.
	dir := t.TempDir()
	p := modelPin{File: "m.bin", URL: "http://127.0.0.1:1/m.bin", Size: 4, SHA256: strings.Repeat("0", 64)}
	dest := filepath.Join(dir, p.File)
	_ = os.WriteFile(dest, []byte("abcd"), 0o644)
	if err := ensureModel(context.Background(), p, dest, func(int64) {}); err == nil {
		t.Fatal("a file failing its checksum was accepted")
	}
}

// A restart re-queues pending work with the channel's current voice-only
// choice, which the segment itself does not record.
func TestRequeuePendingUsesChannelConfig(t *testing.T) {
	s := newTestStore(t)
	r := addTestSegment(t, s, chords(24000, 12), 24000, 25)
	if _, err := s.updateAnalysis(r.ID, func(a *segmentAnalysis) {
		a.Classification = &classificationResult{Status: analysisPending}
		a.Transcript = &transcriptResult{Status: analysisPending, Language: "auto"}
	}); err != nil {
		t.Fatal(err)
	}
	set := fakeWhisperSettings(t, "ok")
	set.Classify, set.ClassifyModel = true, "dsp"
	az := newAnalyzer(set, s, s.hub)
	az.whisper = readyRunner(t, set)
	az.channelLookup = func(string) (AnalysisConfig, int, bool) {
		return AnalysisConfig{Classify: true, Transcribe: true, VoiceOnly: true}, 0, true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go az.classifyWorker()
	go az.transcribeWorker(ctx)
	az.requeuePending()
	waitFor(t, "transcript verdict", func() bool {
		a := analysisOf(s, r.ID)
		return a.Transcript != nil && a.Transcript.Status != analysisPending
	})
	if tr := analysisOf(s, r.ID).Transcript; tr.Status != analysisSkipped || tr.Reason != "no voice detected" {
		t.Fatalf("%+v", tr)
	}
}
