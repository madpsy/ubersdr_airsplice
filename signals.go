// signals.go — naming the signal in a segment: FT8, CW, RTTY...
//
// UberSDR's v2 page names signals from the same EfficientAT mn05 network this
// classifier runs: besides the 527 AudioSet tags, the network computes a
// 640-number embedding of every 5 s window, and the receiver serves
// /v2/audioclass/signals.bin, the same embedding for some 1,800 reference
// recordings of about 500 signals with the rule for when to name one. This is
// that page's scoring (static/v2/src/lib/audioClass/signals.js in
// ka9q_ubersdr), in Go, run on a closed segment rather than a live stream.
//
// References are one network's embeddings and mean nothing to another's, so
// there is a file per model, built into this binary: signals_<model>.bin
// beside this file: signals_mn10.bin for the default model and
// signals_mn05.bin (UberSDR's own signals.bin), both made by
// ubersdr-classifier's train/export_signals.py. Identification runs whenever
// CLASSIFY_MODEL has a file; its dims and header "model" are checked against
// the network.
//
// How a segment is named, as the page does it:
//   - 5 s windows every 2.5 s at 32 kHz; digitally silent ones are skipped;
//   - each window's embedding, minus the references' mean, made unit; summed
//     over a minute of windows (the page's span: signalRunWindows) and made
//     unit again;
//   - its cosine with every reference, the best per signal, a softmax with the
//     file's temperature: each signal's probability, summed into families;
//   - a name when the top signal has t_name of the probability and the windows
//     are near enough that signal's centroid (s_min), else a family (FSK,
//     PSK...) when one has t_family, else nothing — and never before
//     signalMinWindows windows;
//   - voice instead when enough windows are AudioSet speech: at least
//     voice.windows of them, and a quarter of all windows to outweigh a name.
//
// A segment longer than a minute is scored a minute at a time, so a channel
// that carries FT8 and then a voice net is not blurred into nothing; the
// segment is called after the signal most of its minutes name (signalRuns).
package main

import (
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
)

const (
	signalsMagic    = 0x47534255 // "UBSG", little-endian
	signalsVersion  = 1
	signalWindowSec = 5.0
	signalHopSec    = 2.5
	// signalMinWindows: before this many windows nothing is named, as on the page.
	signalMinWindows = 4
	// signalRunWindows: the windows scored together, a minute's worth: the
	// page's MAX_WINDOWS. A shorter tail joins the run before it when it has
	// under half this.
	signalRunWindows = 24
	// signalSilentRMS: a window quieter than this is digital silence (squelch),
	// not a signal; the page's floor.
	signalSilentRMS = 1e-4
)

type signalsHeader struct {
	Model string  `json:"model"`
	Dims  int     `json:"dims"`
	Tau   float64 `json:"tau"`
	Rule  struct {
		TName   float64 `json:"t_name"`
		SMin    float64 `json:"s_min"`
		TFamily float64 `json:"t_family"`
	} `json:"rule"`
	Voice struct {
		Conf    float64 `json:"conf"`
		Windows int     `json:"windows"`
		Strong  float64 `json:"strong"`
	} `json:"voice"`
	Signals []struct {
		Name   string `json:"name"`
		Short  string `json:"short"`
		Family string `json:"family"`
	} `json:"signals"`
	Families []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"families"`
	References struct {
		Total int `json:"total"`
	} `json:"references"`
}

// signalRefs is signals.bin, parsed.
type signalRefs struct {
	hdr     signalsHeader
	d, s, n int
	mean    []float32 // [d]
	refs    []float32 // [n][d], unit
	refSig  []uint16  // [n]
	cent    []float32 // [s][d], unit
	sigFam  []int     // [s]
}

func f16(h uint16) float32 {
	sign := float32(1)
	if h&0x8000 != 0 {
		sign = -1
	}
	e := int(h>>10) & 0x1f
	f := float64(h & 0x3ff)
	switch e {
	case 0:
		return sign * float32(f*math.Pow(2, -24))
	case 31:
		if f != 0 {
			return float32(math.NaN())
		}
		return sign * float32(math.Inf(1))
	}
	return sign * float32((1+f/1024)*math.Pow(2, float64(e-15)))
}

// parseSignals reads signals.bin (format version 1, see ubersdr-classifier's
// docs/SIGNALS_BIN.md) for the model whose weights are `weights` (the .pt
// file's stem, as the header names them; "" accepts any). It refuses anything
// malformed or truncated, and references made with other weights.
func parseSignals(b []byte, weights string) (*signalRefs, error) {
	if len(b) < 12 || binary.LittleEndian.Uint32(b) != signalsMagic {
		return nil, errors.New("not a signals file")
	}
	if v := binary.LittleEndian.Uint32(b[4:]); v != signalsVersion {
		return nil, fmt.Errorf("signals file version %d not supported", v)
	}
	hlen := int(binary.LittleEndian.Uint32(b[8:]))
	off := 12
	if hlen <= 0 || off+hlen > len(b) {
		return nil, errors.New("signals file truncated")
	}
	r := &signalRefs{}
	if err := json.Unmarshal(b[off:off+hlen], &r.hdr); err != nil {
		return nil, fmt.Errorf("signals header: %w", err)
	}
	off += hlen
	h := &r.hdr
	r.d, r.s, r.n = h.Dims, len(h.Signals), h.References.Total
	if r.d <= 0 || r.s == 0 || r.n == 0 || len(h.Families) == 0 || h.Tau <= 0 {
		return nil, errors.New("signals header incomplete")
	}
	if weights != "" && h.Model != weights {
		return nil, fmt.Errorf("references made with %q, not %s", h.Model, weights)
	}
	need := off + 4*r.d + 2*r.n*r.d + 2*r.n + (2*r.n)%4 + 2*r.s*r.d
	if len(b) < need {
		return nil, errors.New("signals file truncated")
	}
	r.mean = make([]float32, r.d)
	for i := range r.mean {
		r.mean[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[off:]))
		off += 4
	}
	half := func(n int) []float32 {
		out := make([]float32, n)
		for i := range out {
			out[i] = f16(binary.LittleEndian.Uint16(b[off:]))
			off += 2
		}
		return out
	}
	r.refs = half(r.n * r.d)
	r.refSig = make([]uint16, r.n)
	for i := range r.refSig {
		r.refSig[i] = binary.LittleEndian.Uint16(b[off:])
		if int(r.refSig[i]) >= r.s {
			return nil, errors.New("signals file: reference for an unknown signal")
		}
		off += 2
	}
	off += (2 * r.n) % 4
	r.cent = half(r.s * r.d)
	fam := map[string]int{}
	for i, f := range h.Families {
		fam[f.ID] = i
	}
	r.sigFam = make([]int, r.s)
	for i, s := range h.Signals {
		r.sigFam[i] = fam[s.Family] // unknown: the first, as the page does
	}
	return r, nil
}

// signalResult is what a segment's windows add up to, under
// classification.signal.
type signalResult struct {
	// Say is the rule's call: name (Name is it), family (FamilyText is it),
	// voice (AudioSet heard speech), or nothing.
	Say        string        `json:"say"`
	Name       string        `json:"name,omitempty"`  // the closest signal, named or not
	Short      string        `json:"short,omitempty"` // its badge text, e.g. "CW"
	P          float64       `json:"p"`
	Family     string        `json:"family,omitempty"`
	FamilyText string        `json:"family_text,omitempty"` // e.g. "FSK"
	FamilyP    float64       `json:"family_p"`
	Top        []signalScore `json:"top,omitempty"` // the three closest signals
	Windows    int           `json:"windows"`       // windows scored (silent ones are not)
	Speech     int           `json:"speech"`        // of which AudioSet heard as speech
	// Runs are the segment's minutes, each scored on its own, in order; only
	// for a segment longer than one. The fields above are then the run the
	// segment is called after, with Windows and Speech over all of them.
	Runs []signalRun `json:"runs,omitempty"`
}

// signalRun is one minute's call.
type signalRun struct {
	StartSec   float64 `json:"start_sec"` // offset of its first window into the segment
	Windows    int     `json:"windows"`
	Say        string  `json:"say"`
	Name       string  `json:"name,omitempty"` // for say name: the signal
	Short      string  `json:"short,omitempty"`
	FamilyText string  `json:"family_text,omitempty"` // for say family
	P          float64 `json:"p"`                     // how likely what it said (name or family)
}

// identified reports whether the segment was named, or given a family.
func (r *signalResult) identified() bool {
	return r != nil && (r.Say == "name" || r.Say == "family")
}

// identified reports, for a segment of several minutes, whether any minute
// named a signal or a family.
func (r *signalResult) anyIdentified() bool {
	if r.identified() {
		return true
	}
	for _, run := range r.runs() {
		if run.Say == "name" || run.Say == "family" {
			return true
		}
	}
	return false
}

func (r *signalResult) runs() []signalRun {
	if r == nil {
		return nil
	}
	return r.Runs
}

// counts are the segment's entries in a recording's summary (Segments unset):
// every signal or family any of its minutes identified, once each.
func (r *signalResult) counts() []signalCount {
	if r == nil {
		return nil
	}
	runs := r.Runs
	if len(runs) == 0 {
		runs = []signalRun{r.asRun()}
	}
	var out []signalCount
	seen := map[signalCount]bool{}
	for _, run := range runs {
		var k signalCount
		switch run.Say {
		case "name":
			k = signalCount{Signal: run.Short, Name: run.Name}
		case "family":
			k = signalCount{Signal: run.FamilyText, Name: run.FamilyText, Family: true}
		default:
			continue
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func (r *signalResult) asRun() signalRun {
	run := signalRun{Windows: r.Windows, Say: r.Say}
	switch r.Say {
	case "name":
		run.Name, run.Short, run.P = r.Name, r.Short, r.P
	case "family":
		run.FamilyText, run.P = r.FamilyText, r.FamilyP
	}
	return run
}

type signalScore struct {
	Name string  `json:"name"`
	P    float64 `json:"p"`
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// signalAccum sums a segment's windows as the page's tracker does.
type signalAccum struct {
	ref    *signalRefs
	sum    []float64
	n      int
	speech int
	w      []float32
}

func newSignalAccum(ref *signalRefs) *signalAccum {
	return &signalAccum{ref: ref, sum: make([]float64, ref.d), w: make([]float32, ref.d)}
}

func (a *signalAccum) reset() {
	clear(a.sum)
	a.n, a.speech = 0, 0
}

// add takes one window's embedding and its voice / music / noise verdict.
func (a *signalAccum) add(emb []float32, label string, conf float64) {
	var nn float64
	for i := range a.w {
		a.w[i] = emb[i] - a.ref.mean[i]
		nn += float64(a.w[i]) * float64(a.w[i])
	}
	k := 1 / (math.Sqrt(nn) + 1e-9)
	for i, v := range a.w {
		a.sum[i] += float64(v) * k
	}
	a.n++
	if label == "voice" && conf >= a.ref.hdr.Voice.Conf {
		a.speech++
	}
}

// result scores the windows added so far; nil when there were none.
func (a *signalAccum) result() *signalResult {
	if a.n == 0 {
		return nil
	}
	ref, h := a.ref, &a.ref.hdr
	d := ref.d
	v := make([]float64, d)
	var nn float64
	for _, x := range a.sum {
		nn += x * x
	}
	k := 1 / (math.Sqrt(nn) + 1e-9)
	for i, x := range a.sum {
		v[i] = x * k
	}
	dot := func(m []float32, row int) float64 {
		var s float64
		for i, x := range m[row*d : (row+1)*d] {
			s += float64(x) * v[i]
		}
		return s
	}
	best := make([]float64, ref.s)
	for i := range best {
		best[i] = math.Inf(-1)
	}
	for r := 0; r < ref.n; r++ {
		if s := dot(ref.refs, r); s > best[ref.refSig[r]] {
			best[ref.refSig[r]] = s
		}
	}
	mx := math.Inf(-1)
	for _, b := range best {
		mx = math.Max(mx, b)
	}
	p := make([]float64, ref.s)
	var tot float64
	for i, b := range best {
		if !math.IsInf(b, -1) {
			p[i] = math.Exp((b - mx) / h.Tau)
			tot += p[i]
		}
	}
	fp := make([]float64, len(h.Families))
	top := 0
	for i := range p {
		p[i] /= tot
		fp[ref.sigFam[i]] += p[i]
		if p[i] > p[top] {
			top = i
		}
	}
	ftop := 0
	for f := range fp {
		if fp[f] > fp[ftop] {
			ftop = f
		}
	}
	closeness := dot(ref.cent, top)

	say := "nothing"
	if a.n >= signalMinWindows {
		if p[top] >= h.Rule.TName && closeness >= h.Rule.SMin {
			say = "name"
		} else if fp[ftop] >= h.Rule.TFamily {
			say = "family"
		}
	}
	if a.speech >= h.Voice.Windows && (say != "name" || float64(a.speech) >= h.Voice.Strong*float64(a.n)) {
		say = "voice"
	}

	order := make([]int, ref.s)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return p[order[i]] > p[order[j]] })
	res := &signalResult{
		Say: say, Name: h.Signals[top].Name, Short: h.Signals[top].Short, P: round3(p[top]),
		Family: h.Families[ftop].ID, FamilyText: h.Families[ftop].Text, FamilyP: round3(fp[ftop]),
		Windows: a.n, Speech: a.speech,
	}
	for _, i := range order[:min(3, len(order))] {
		res.Top = append(res.Top, signalScore{h.Signals[i].Name, round3(p[i])})
	}
	return res
}

// identifyFrom names the signal in a stream already at 32 kHz: 5 s windows
// every 2.5 s through the model, scored against ref. A segment shorter than a
// window is one window, zero-padded, as the references were made.
func (m *eatModel) identifyFrom(r *resampledReader, ref *signalRefs) (*signalResult, error) {
	if m.hidden != ref.d {
		return nil, fmt.Errorf("the signal references have %d dimensions; %s's embedding has %d", ref.d, m.name, m.hidden)
	}
	win, hop := int(signalWindowSec*eatRate), int(signalHopSec*eatRate)
	// Each window's embedding and verdict, in order: a few kB per window, so
	// an hour is a few MB.
	type window struct {
		at    float64
		emb   []float32
		label string
		conf  float64
	}
	var ws []window
	pos := 0
	visit := func(x []float32) {
		at := float64(pos) / eatRate
		var ss float64
		for _, v := range x {
			ss += float64(v) * float64(v)
		}
		if math.Sqrt(ss/float64(len(x))) < signalSilentRMS {
			return
		}
		probs, emb := m.forward(eatMelSpec(x))
		label, conf, _ := eatWindowVerdict(probs)
		ws = append(ws, window{at, emb, label, conf})
	}
	buf := make([]float32, 0, win)
	windows := 0
	for {
		want := win - len(buf)
		x, err := r.next(want)
		if err != nil {
			return nil, err
		}
		buf = append(buf, x...)
		if len(buf) < win {
			// The end. Nothing whole was heard: pad what there is.
			if windows == 0 && len(buf) > 0 {
				visit(append(buf, make([]float32, win-len(buf))...))
			}
			break
		}
		visit(buf)
		windows++
		pos += hop
		buf = append(buf[:0], buf[hop:]...)
	}
	if len(ws) == 0 {
		return nil, nil
	}
	// A minute at a time; a short tail joins the minute before it.
	var bounds []int
	for i := 0; i < len(ws); i += signalRunWindows {
		bounds = append(bounds, i)
	}
	if n := len(bounds); n > 1 && len(ws)-bounds[n-1] < signalRunWindows/2 {
		bounds = bounds[:n-1]
	}
	acc := newSignalAccum(ref)
	var results []*signalResult
	var starts []float64
	for k, lo := range bounds {
		hi := len(ws)
		if k+1 < len(bounds) {
			hi = bounds[k+1]
		}
		acc.reset()
		for _, w := range ws[lo:hi] {
			acc.add(w.emb, w.label, w.conf)
		}
		results = append(results, acc.result())
		starts = append(starts, ws[lo].at)
	}
	return combineRuns(results, starts), nil
}

// combineRuns calls a segment from its minutes' results. One minute is the
// call itself. Of several, the call is the signal most minutes named (the
// likelier on a tie), else the family most named, else voice if most minutes
// said so, else nothing; its fields are the minute that said it most surely.
func combineRuns(results []*signalResult, starts []float64) *signalResult {
	if len(results) == 1 {
		return results[0]
	}
	type tally struct {
		n    int
		best *signalResult
		bp   float64
	}
	pick := func(say string, key func(*signalResult) string, p func(*signalResult) float64) *signalResult {
		t := map[string]*tally{}
		var order []string
		for _, r := range results {
			if r.Say != say {
				continue
			}
			k := key(r)
			if t[k] == nil {
				t[k] = &tally{}
				order = append(order, k)
			}
			t[k].n++
			if p(r) > t[k].bp {
				t[k].best, t[k].bp = r, p(r)
			}
		}
		var win *tally
		for _, k := range order {
			if c := t[k]; win == nil || c.n > win.n || (c.n == win.n && c.bp > win.bp) {
				win = c
			}
		}
		if win == nil {
			return nil
		}
		return win.best
	}
	best := pick("name", func(r *signalResult) string { return r.Name }, func(r *signalResult) float64 { return r.P })
	if best == nil {
		best = pick("family", func(r *signalResult) string { return r.FamilyText }, func(r *signalResult) float64 { return r.FamilyP })
	}
	voice := 0
	for _, r := range results {
		if r.Say == "voice" {
			voice++
		}
	}
	if best == nil && voice*2 > len(results) {
		best = pick("voice", func(*signalResult) string { return "" }, func(r *signalResult) float64 { return float64(r.Speech) / float64(r.Windows) })
	}
	var out signalResult
	if best != nil {
		out = *best
	} else {
		// Nothing: the minute that came closest to naming something.
		out = *results[0]
		for _, r := range results[1:] {
			if r.P > out.P {
				out = *r
			}
		}
		out.Say = "nothing"
	}
	out.Windows, out.Speech = 0, 0
	for i, r := range results {
		out.Windows += r.Windows
		out.Speech += r.Speech
		run := r.asRun()
		run.StartSec = math.Round(starts[i]*10) / 10
		out.Runs = append(out.Runs, run)
	}
	return &out
}

// ── the references, built in ─────────────────────────────────────────────────

// signalFiles are the references per model, signals_<model>.bin. Both are
// named, so a build (the Docker image's included) fails without either rather
// than shipping without identification for that model. Replace a file and
// rebuild to update it; to add a model, add its file here too.
//
//go:embed signals_mn10.bin signals_mn05.bin
var signalFiles embed.FS

var (
	signalsMu  sync.Mutex
	signalsFor = map[string]*signalsLoad{}
)

type signalsLoad struct {
	ref *signalRefs
	err error
}

// signalsFile is the built-in references file for a model, or nil.
func signalsFile(model string) []byte {
	b, err := signalFiles.ReadFile("signals_" + model + ".bin")
	if err != nil {
		return nil
	}
	return b
}

// hasSignals reports whether this build carries references for a model.
func hasSignals(model string) bool {
	_, err := fs.Stat(signalFiles, "signals_"+model+".bin")
	return err == nil
}

// signalModels are the models this build can identify signals with.
func signalModels() []string {
	var out []string
	for m := range eatModels {
		if hasSignals(m) {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// builtinSignals parses a model's built-in references, once.
func builtinSignals(model string) (*signalRefs, error) {
	signalsMu.Lock()
	defer signalsMu.Unlock()
	if l := signalsFor[model]; l != nil {
		return l.ref, l.err
	}
	l := &signalsLoad{}
	if b := signalsFile(model); b == nil {
		l.err = fmt.Errorf("no signal references for %s in this build", model)
	} else {
		l.ref, l.err = parseSignals(b, strings.TrimSuffix(eatModels[model].pin.File, ".pt"))
		if l.err != nil {
			log.Printf("[identify] built-in references for %s unusable: %v", model, l.err)
		}
	}
	signalsFor[model] = l
	return l.ref, l.err
}
