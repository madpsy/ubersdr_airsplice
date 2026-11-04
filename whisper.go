// whisper.go — transcription through UberSDR's whisper-helper.
//
// whisper-helper (clients/electron/whisper in the UberSDR tree) is whisper.cpp
// with Silero VAD, linked statically, speaking a framed protocol on stdin and
// JSON lines on stdout; the protocol is documented at the top of
// whisper_helper.cpp. It was written for live audio, which shapes how this
// side drives it:
//
//   - It drops audio once it is more than 10 s behind, and says so once with a
//     non-fatal "falling-behind" error, but never says how far through it is.
//     A finished segment is therefore fed at a set pace, never all at once.
//     With WHISPER_SPEED=auto (the default) the pace adapts: it starts at 2x
//     real time and rises by a quarter after each segment the helper keeps up
//     with, to at most 8x; when the helper falls behind, that attempt is
//     abandoned at once, the pace halved, and the segment transcribed again,
//     so adapting never costs a transcript. Only a helper that cannot keep up
//     even at 1x leaves a transcript marked partial. A fixed WHISPER_SPEED
//     feeds at that pace and marks what the helper fell behind on partial.
//   - It transcribes an utterance when the VAD decides it has ended, and STOP
//     does not flush one in progress. So each segment is followed by a second
//     and a half of silence, and the session is held open until the finals
//     stop arriving.
//
// One helper process serves every channel, one segment at a time, and is kept
// running so the model is loaded once. It runs at low CPU priority.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// modelPin is a model file pinned by URL, size and SHA-256: the entries of
// clients/electron/whisper/models.json in the UberSDR tree. Each URL names a
// Hugging Face commit, so a file replaced upstream cannot change what is used.
type modelPin struct {
	File   string
	URL    string
	Size   int64
	SHA256 string
}

var whisperModels = map[string]modelPin{
	"tiny": {
		File:   "ggml-tiny-q5_1.bin",
		URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-tiny-q5_1.bin",
		Size:   32152673,
		SHA256: "818710568da3ca15689e31a743197b520007872ff9576237bda97bd1b469c3d7",
	},
	"base": {
		File:   "ggml-base-q5_1.bin",
		URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base-q5_1.bin",
		Size:   59707625,
		SHA256: "422f1ae452ade6f30a004d7e5c6a43195e4433bc370bf23fac9cc591f01a8898",
	},
	"small": {
		File:   "ggml-small-q5_1.bin",
		URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-small-q5_1.bin",
		Size:   190085487,
		SHA256: "ae85e4a935d7a567bd102fe55afc16bb595bdb618e11b2fc7591bc08120411bb",
	},
}

var vadModel = modelPin{
	File:   "ggml-silero-v6.2.0.bin",
	URL:    "https://huggingface.co/ggml-org/whisper-vad/resolve/9ffd54a1e1ee413ddf265af9913beaf518d1639b/ggml-silero-v6.2.0.bin",
	Size:   885098,
	SHA256: "2aa269b785eeb53a82983a20501ddf7c1d9c48e33ab63a41391ac6c9f7fb6987",
}

func whisperModelNames() []string {
	names := make([]string, 0, len(whisperModels))
	for k := range whisperModels {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return whisperModels[names[i]].Size < whisperModels[names[j]].Size })
	return names
}

// Helper frame types (whisper_helper.cpp).
const (
	frameStart = 1
	frameAudio = 2
	frameStop  = 4
)

const (
	whisperFrameSamples = analysisRate / 10    // 100 ms per AUDIO frame
	whisperTailSilence  = analysisRate * 3 / 2 // 1.5 s to close the last utterance
	whisperMaxSettle    = 60 * time.Second     // give up waiting after the audio
	whisperReadyTimeout = 2 * time.Minute      // model load
)

// whisperSettle: once the audio is in, no new final for this long means the
// segment is done. A variable so the tests need not wait for it.
var whisperSettle = 4 * time.Second

type helperLine struct {
	Type    string `json:"type"`
	Session *int64 `json:"session"`
	Text    string `json:"text"`
	Final   bool   `json:"final"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

type whisperRunner struct {
	settings analysisSettings

	mu        sync.Mutex
	state     string // idle | starting | downloading | ready | unavailable | failed
	once      sync.Once
	errMsg    string
	progress  float64 // download progress 0..1
	languages []string
	readyCh   chan struct{} // closed once prepare has finished, whatever the outcome

	helperPath, modelPath, vadPath string

	speed float64 // current feed pace, x real time (adaptive unless WHISPER_SPEED is set)

	// procMu serialises use of the helper process. It is held for a whole
	// segment, so it is separate from mu, which status() takes.
	procMu  sync.Mutex
	proc    *exec.Cmd
	stdin   io.WriteCloser
	lines   chan helperLine
	session int64
}

func newWhisperRunner(s analysisSettings) *whisperRunner {
	speed := s.WhisperSpeed
	if speed <= 0 { // auto
		speed = whisperAutoStart
	}
	return &whisperRunner{settings: s, state: "idle", readyCh: make(chan struct{}), speed: speed}
}

// The adaptive pace (WHISPER_SPEED=auto): where it starts, its bounds, and
// how far it rises after a segment the helper kept up with.
const (
	whisperAutoStart = 2.0
	whisperAutoMin   = 1.0
	whisperAutoMax   = 8.0
	whisperAutoStep  = 1.25
)

func (w *whisperRunner) adaptive() bool { return w.settings.WhisperSpeed <= 0 }

func (w *whisperRunner) currentSpeed() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.speed
}

func (w *whisperRunner) setSpeed(v float64) {
	w.mu.Lock()
	w.speed = math.Max(whisperAutoMin, math.Min(whisperAutoMax, v))
	w.mu.Unlock()
}

// begin starts prepare the first time something needs transcription: a
// channel opting in, or a segment queued. Until then nothing is downloaded.
func (w *whisperRunner) begin(ctx context.Context) {
	w.once.Do(func() { go w.prepare(ctx) })
}

func (w *whisperRunner) setState(state, msg string) {
	w.mu.Lock()
	w.state, w.errMsg = state, msg
	w.mu.Unlock()
	if msg != "" {
		log.Printf("[whisper] %s: %s", state, msg)
	} else {
		log.Printf("[whisper] %s", state)
	}
}

func (w *whisperRunner) status() map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]interface{}{"state": w.state, "speed": math.Round(w.speed*10) / 10, "adaptive": w.adaptive()}
	if w.errMsg != "" {
		out["error"] = w.errMsg
	}
	if w.state == "downloading" {
		out["progress"] = w.progress
	}
	if len(w.languages) > 0 {
		out["languages"] = w.languages
	}
	return out
}

// prepare finds the helper and makes sure both models are on disk. It runs
// once at startup, in the background; transcription waits for it.
func (w *whisperRunner) prepare(ctx context.Context) {
	defer close(w.readyCh)
	w.mu.Lock()
	w.state = "starting"
	w.mu.Unlock()
	path, err := exec.LookPath(w.settings.WhisperHelper)
	if err != nil {
		w.setState("unavailable", fmt.Sprintf("whisper-helper not found (%q); set WHISPER_HELPER to its path", w.settings.WhisperHelper))
		return
	}
	w.helperPath = path
	if out, err := exec.CommandContext(ctx, path, "--version").Output(); err == nil {
		log.Printf("[whisper] %s", strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, path, "--languages").Output(); err == nil {
		var langs []string
		if json.Unmarshal(out, &langs) == nil {
			w.mu.Lock()
			w.languages = langs
			w.mu.Unlock()
		}
	}
	dir := w.settings.ModelDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.setState("failed", fmt.Sprintf("model dir: %v", err))
		return
	}
	pin := whisperModels[w.settings.WhisperModel]
	total := float64(pin.Size + vadModel.Size)
	done := 0.0
	for _, p := range []modelPin{vadModel, pin} {
		dest := filepath.Join(dir, p.File)
		base := done
		err := ensureModel(ctx, p, dest, func(n int64) {
			w.mu.Lock()
			w.state = "downloading"
			w.progress = (base + float64(n)) / total
			w.mu.Unlock()
		})
		if err != nil {
			w.setState("failed", fmt.Sprintf("%s: %v", p.File, err))
			return
		}
		done += float64(p.Size)
	}
	w.modelPath = filepath.Join(dir, pin.File)
	w.vadPath = filepath.Join(dir, vadModel.File)
	w.setState("ready", "")
}

// ensureModel downloads p to dest unless a file of the right size and hash is
// already there. The download goes to a temporary file, renamed only once it
// has been checked.
func ensureModel(ctx context.Context, p modelPin, dest string, progress func(int64)) error {
	if fi, err := os.Stat(dest); err == nil && fi.Size() == p.Size {
		if sum, err := fileSHA256(dest); err == nil && sum == p.SHA256 {
			return nil
		}
		log.Printf("[models] %s fails its checksum; downloading again", filepath.Base(dest))
	}
	log.Printf("[models] downloading %s (%.1f MB)", p.File, float64(p.Size)/1e6)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	var n int64
	buf := make([]byte, 256<<10)
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			if n+int64(k) > p.Size {
				f.Close()
				os.Remove(tmp)
				return errors.New("download larger than pinned size")
			}
			if _, err := f.Write(buf[:k]); err != nil {
				f.Close()
				os.Remove(tmp)
				return err
			}
			h.Write(buf[:k])
			n += int64(k)
			progress(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if n != p.Size {
		os.Remove(tmp)
		return fmt.Errorf("downloaded %d bytes, pinned size is %d", n, p.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != p.SHA256 {
		os.Remove(tmp)
		return fmt.Errorf("sha256 %s does not match the pinned %s", sum, p.SHA256)
	}
	return os.Rename(tmp, dest)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// startHelper launches the helper and waits for {"type":"ready"}.
func (w *whisperRunner) startHelper() error {
	cmd := exec.Command(w.helperPath, "--model", w.modelPath, "--vad-model", w.vadPath,
		"--threads", fmt.Sprint(w.settings.WhisperThreads))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = &prefixWriter{prefix: "[whisper-helper] "}
	if err := cmd.Start(); err != nil {
		return err
	}
	setLowPriority(cmd.Process.Pid)
	lines := make(chan helperLine, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var l helperLine
			if json.Unmarshal(sc.Bytes(), &l) == nil {
				lines <- l
			}
		}
		close(lines)
	}()
	w.proc, w.stdin, w.lines = cmd, stdin, lines

	timeout := time.After(whisperReadyTimeout)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				w.stopHelper()
				return errors.New("whisper-helper exited while loading its models")
			}
			if l.Type == "ready" {
				log.Printf("[whisper] helper ready (pid %d)", cmd.Process.Pid)
				return nil
			}
			if l.Type == "error" && l.Fatal {
				w.stopHelper()
				return fmt.Errorf("whisper-helper: %s", l.Message)
			}
		case <-timeout:
			w.stopHelper()
			return errors.New("whisper-helper did not become ready")
		}
	}
}

func (w *whisperRunner) stopHelper() {
	if w.stdin != nil {
		w.stdin.Close() // end of input: the helper exits 0
	}
	if w.proc != nil {
		done := make(chan struct{})
		go func(p *exec.Cmd) { _ = p.Wait(); close(done) }(w.proc)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = w.proc.Process.Kill()
			<-done
		}
	}
	w.proc, w.stdin, w.lines = nil, nil, nil
}

func (w *whisperRunner) shutdown() {
	w.procMu.Lock()
	defer w.procMu.Unlock()
	w.stopHelper()
}

func writeFrame(wr io.Writer, typ byte, payload []byte) error {
	var head [5]byte
	head[0] = typ
	binary.LittleEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := wr.Write(head[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := wr.Write(payload)
		return err
	}
	return nil
}

// transcribe runs one segment through the helper, adapting the pace when
// WHISPER_SPEED is auto (see the top of this file).
func (w *whisperRunner) transcribe(ctx context.Context, j analysisJob) *transcriptResult {
	if !w.adaptive() {
		res, _ := w.transcribeOnce(ctx, j, w.settings.WhisperSpeed, false)
		return res
	}
	for {
		speed := w.currentSpeed()
		// Above 1x, an attempt the helper falls behind on is abandoned and
		// retried slower; at 1x there is nothing slower, so it completes.
		res, behind := w.transcribeOnce(ctx, j, speed, speed > whisperAutoMin)
		if behind {
			w.setSpeed(speed / 2)
			log.Printf("[whisper] fell behind at %.1fx real time; retrying the segment at %.1fx", speed, w.currentSpeed())
			continue
		}
		if res.Status == analysisDone && !res.Partial {
			w.setSpeed(speed * whisperAutoStep)
		}
		return res
	}
}

// transcribeOnce feeds one segment at `speed` x real time. With abortOnBehind
// it stops as soon as the helper reports falling behind and returns behind
// true (the result is then meaningless); otherwise a transcript the helper
// fell behind on is marked partial.
func (w *whisperRunner) transcribeOnce(ctx context.Context, j analysisJob, speed float64, abortOnBehind bool) (*transcriptResult, bool) {
	res := &transcriptResult{Model: w.settings.WhisperModel, Language: langOrAuto(j.cfg.Language), Speed: speed}
	fail := func(status, reason string) *transcriptResult {
		res.Status, res.Reason, res.At = status, reason, time.Now().UTC()
		return res
	}
	fail2 := func(status, reason string) (*transcriptResult, bool) { return fail(status, reason), false }
	w.begin(ctx)
	select {
	case <-w.readyCh:
	case <-ctx.Done():
		return fail2(analysisSkipped, "shutting down")
	}
	w.mu.Lock()
	state, errMsg := w.state, w.errMsg
	w.mu.Unlock()
	if state != "ready" {
		return fail2(analysisFailed, "transcription unavailable: "+errMsg)
	}

	wav, err := openWAVStream(j.wavPath)
	if err != nil {
		return fail2(analysisFailed, err.Error())
	}
	defer wav.Close()
	audio := newResampledReader(wav, wav.rate, analysisRate) // streamed, 100 ms at a time

	w.procMu.Lock()
	defer w.procMu.Unlock()
	if w.proc == nil {
		if err := w.startHelper(); err != nil {
			log.Printf("[whisper] %v", err)
			return fail2(analysisFailed, err.Error())
		}
	}
	w.session++
	sess := w.session
	start, _ := json.Marshal(map[string]interface{}{"lang": res.Language, "session": sess})
	if err := writeFrame(w.stdin, frameStart, start); err != nil {
		w.stopHelper()
		return fail2(analysisFailed, "helper: "+err.Error())
	}

	var finals []string
	partial := false
	var fatal error
	handle := func(l helperLine) {
		if l.Session != nil && *l.Session != sess {
			return // a line from an earlier session
		}
		switch l.Type {
		case "result":
			if l.Final && strings.TrimSpace(l.Text) != "" {
				finals = append(finals, strings.TrimSpace(l.Text))
			}
		case "error":
			if l.Code == "falling-behind" {
				partial = true
			} else if l.Fatal {
				fatal = errors.New(l.Message)
			} else {
				log.Printf("[whisper] %s: %s", l.Code, l.Message)
			}
		}
	}
	// drain reads whatever the helper has said so far without blocking.
	drain := func() bool {
		for {
			select {
			case l, ok := <-w.lines:
				if !ok {
					fatal = errors.New("whisper-helper exited")
					return false
				}
				handle(l)
			default:
				return true
			}
		}
	}

	// Feed the audio at `speed` x real time. With a voice mask, only the
	// windows the classifier heard voice in are sent; each run of skipped
	// windows is marked by a short silence so the utterance before it closes.
	// Skipped audio costs no time. Then the closing silence.
	pcm := make([]byte, whisperFrameSamples*2)
	frameDur := time.Duration(float64(time.Second) * float64(whisperFrameSamples) / analysisRate / speed)
	t0 := time.Now()
	sent := 0 // frames sent, for pacing
	const (
		feedOK = iota
		feedEnd
		feedBehind
		feedErr
	)
	var sendErr error
	send := func(frame []float32) int {
		for s, v := range frame {
			binary.LittleEndian.PutUint16(pcm[2*s:], uint16(floatToInt16(v)))
		}
		if err := writeFrame(w.stdin, frameAudio, pcm[:2*len(frame)]); err != nil {
			sendErr = err
			return feedErr
		}
		if !drain() || fatal != nil {
			return feedEnd
		}
		if partial && abortOnBehind {
			return feedBehind
		}
		sent++
		if wait := time.Until(t0.Add(time.Duration(sent) * frameDur)); wait > 0 {
			time.Sleep(wait)
		}
		return feedOK
	}
	silenceFrames := func(n int) int {
		for n > 0 {
			f := make([]float32, min(whisperFrameSamples, n))
			n -= len(f)
			if r := send(f); r != feedOK {
				return r
			}
		}
		return feedOK
	}
	pos, skipped := 0, 0 // samples read / skipped as not voice
	inGap, anyVoice := false, false
	status := feedOK
	for status == feedOK {
		if ctx.Err() != nil {
			_ = writeFrame(w.stdin, frameStop, nil)
			return fail2(analysisSkipped, "shutting down")
		}
		frame, rerr := audio.next(whisperFrameSamples)
		if rerr != nil {
			_ = writeFrame(w.stdin, frameStop, nil)
			return fail2(analysisFailed, rerr.Error())
		}
		if len(frame) == 0 {
			status = silenceFrames(whisperTailSilence)
			break
		}
		voice := j.voice.at(pos)
		pos += len(frame)
		if !voice {
			skipped += len(frame)
			if anyVoice && !inGap {
				inGap = true
				status = silenceFrames(whisperTailSilence) // close the utterance before the gap
			}
			continue
		}
		inGap, anyVoice = false, true
		status = send(frame)
	}
	switch status {
	case feedErr:
		w.stopHelper()
		return fail2(analysisFailed, "helper: "+sendErr.Error())
	case feedBehind:
		_ = writeFrame(w.stdin, frameStop, nil) // its remaining lines are ignored by session
		return res, true
	}
	res.SkippedSec = math.Round(float64(skipped)/analysisRate*10) / 10

	// Wait for the last finals: done once nothing has arrived for a while.
	deadline := time.Now().Add(whisperMaxSettle)
	settle := time.NewTimer(whisperSettle)
	defer settle.Stop()
wait:
	for fatal == nil && time.Now().Before(deadline) {
		select {
		case l, ok := <-w.lines:
			if !ok {
				fatal = errors.New("whisper-helper exited")
				break wait
			}
			handle(l)
			if l.Type == "result" {
				if !settle.Stop() {
					select {
					case <-settle.C:
					default:
					}
				}
				settle.Reset(whisperSettle)
			}
		case <-settle.C:
			break wait
		case <-ctx.Done():
			break wait
		}
	}
	if fatal != nil {
		w.stopHelper() // restarted on the next segment
		return fail2(analysisFailed, fatal.Error())
	}
	_ = writeFrame(w.stdin, frameStop, nil)
	if partial && abortOnBehind {
		return res, true
	}

	res.Status = analysisDone
	res.Lines = finals
	res.Text = strings.Join(finals, " ")
	res.Partial = partial
	res.At = time.Now().UTC()
	return res, false
}

func floatToInt16(v float32) int16 {
	x := v * 32767
	if x > 32767 {
		return 32767
	}
	if x < -32768 {
		return -32768
	}
	return int16(x)
}

// prefixWriter logs the helper's stderr line by line.
type prefixWriter struct {
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := strings.IndexByte(string(p.buf), '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(p.buf[:i])); line != "" {
			log.Printf("%s%s", p.prefix, line)
		}
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}
