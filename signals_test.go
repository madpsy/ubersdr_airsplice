package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The built-in references parse, and name the signals the badge names on air.
func TestBuiltinSignals(t *testing.T) {
	ref, err := builtinSignals("mn05")
	if err != nil {
		t.Fatal(err)
	}
	if ref.d != 640 || ref.s < 100 || ref.n < ref.s {
		t.Fatalf("dims %d, %d signals, %d references", ref.d, ref.s, ref.n)
	}
	short := map[string]bool{}
	for _, s := range ref.hdr.Signals {
		short[s.Short] = true
	}
	for _, want := range []string{"FT8", "FT4", "CW", "RTTY"} {
		if !short[want] {
			t.Errorf("no signal with short name %q", want)
		}
	}
	// References and centroids are unit vectors (to half precision).
	for _, m := range []struct {
		name string
		v    []float32
		rows int
	}{{"references", ref.refs, ref.n}, {"centroids", ref.cent, ref.s}} {
		for r := 0; r < m.rows; r += 97 {
			var nn float64
			for _, x := range m.v[r*ref.d : (r+1)*ref.d] {
				nn += float64(x) * float64(x)
			}
			if math.Abs(math.Sqrt(nn)-1) > 0.01 {
				t.Fatalf("%s row %d has norm %.4f", m.name, r, math.Sqrt(nn))
			}
		}
	}
}

func TestParseSignalsRejects(t *testing.T) {
	if _, err := parseSignals([]byte("RIFF...."), ""); err == nil {
		t.Error("accepted a non-signals file")
	}
	if _, err := parseSignals(signalsFile("mn05")[:len(signalsFile("mn05"))/2], ""); err == nil {
		t.Error("accepted a truncated file")
	}
}

func TestF16(t *testing.T) {
	for h, want := range map[uint16]float32{0x3c00: 1, 0xc000: -2, 0x3800: 0.5, 0x0000: 0, 0x0001: 5.9604645e-08} {
		if got := f16(h); got != want {
			t.Errorf("f16(%#04x) = %g, want %g", h, got, want)
		}
	}
}

// A reference's own vector, added as a window, is named as its signal: the
// scoring is the page's.
func TestScoreOwnReference(t *testing.T) {
	ref, err := builtinSignals("mn05")
	if err != nil {
		t.Fatal(err)
	}
	cw := -1
	for i, s := range ref.hdr.Signals {
		if s.Short == "CW" {
			cw = i
		}
	}
	// The CW centroid, put back into embedding space (plus the mean), four times.
	acc := newSignalAccum(ref)
	emb := make([]float32, ref.d)
	for i := range emb {
		emb[i] = ref.cent[cw*ref.d+i] + ref.mean[i]
	}
	for k := 0; k < signalMinWindows; k++ {
		acc.add(emb, "noise", 0.9)
	}
	got := acc.result()
	if got.Say != "name" || got.Short != "CW" {
		t.Fatalf("got %+v", got)
	}
	// Fewer windows than the minimum name nothing.
	acc = newSignalAccum(ref)
	acc.add(emb, "noise", 0.9)
	if got := acc.result(); got.Say != "nothing" {
		t.Fatalf("one window: got %q", got.Say)
	}
	// Speech in a quarter of the windows outweighs the name.
	acc = newSignalAccum(ref)
	for k := 0; k < 8; k++ {
		label := "noise"
		if k < 2 {
			label = "voice"
		}
		acc.add(emb, label, 0.9)
	}
	if got := acc.result(); got.Say != "voice" {
		t.Fatalf("speech: got %q", got.Say)
	}
}

// eachSignalModel runs fn for every model with references whose weights are
// in AIRSPLICE_EAT_MODELS (mn10 and mn05), as a subtest.
func eachSignalModel(t *testing.T, fn func(t *testing.T, m *eatModel, ref *signalRefs)) {
	dir := eatModelDir(t)
	ran := false
	for _, name := range signalModels() {
		path := filepath.Join(dir, eatModels[name].pin.File)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		ran = true
		t.Run(name, func(t *testing.T) {
			m, err := loadEfficientAT(path, name, eatModels[name].width, 4)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := builtinSignals(name)
			if err != nil {
				t.Fatal(err)
			}
			fn(t, m, ref)
		})
	}
	if !ran {
		t.Skip("no weights for a model with references in AIRSPLICE_EAT_MODELS")
	}
}

// Synthetic Morse through the real model is named CW, as UberSDR's page test
// expects of the same network and references.
func TestIdentifySyntheticCW(t *testing.T) {
	eachSignalModel(t, func(t *testing.T, m *eatModel, ref *signalRefs) { identifySyntheticCW(t, m, ref) })
}

func identifySyntheticCW(t *testing.T, m *eatModel, ref *signalRefs) {
	const rate = 12000
	// "CQ CQ TEST" at ~20 wpm on a 700 Hz tone, repeated for 30 s, over light noise.
	morse := map[rune]string{'C': "-.-.", 'Q': "--.-", 'T': "-", 'E': ".", 'S': "..."}
	dot := int(0.06 * rate)
	var keys []bool
	for len(keys) < 30*rate {
		for _, ch := range "CQ CQ TEST " {
			if ch == ' ' {
				keys = append(keys, make([]bool, 4*dot)...)
				continue
			}
			for _, el := range morse[ch] {
				n := dot
				if el == '-' {
					n = 3 * dot
				}
				for i := 0; i < n; i++ {
					keys = append(keys, true)
				}
				keys = append(keys, make([]bool, dot)...)
			}
			keys = append(keys, make([]bool, 2*dot)...)
		}
	}
	keys = keys[:30*rate]
	noise := whiteNoise(len(keys), 0.02, 7)
	x := make([]float32, len(keys))
	env := 0.0
	for i, k := range keys {
		target := 0.0
		if k {
			target = 1
		}
		env += (target - env) * 0.01 // soft keying, no clicks
		x[i] = float32(0.4*env*math.Sin(2*math.Pi*700*float64(i)/rate)) + noise[i]
	}
	got, err := m.identifyFrom(newResampledReader(&sliceSource{x: x}, rate, eatRate), ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", got)
	if got.Say != "name" || got.Short != "CW" {
		t.Fatalf("synthetic CW: said %s %q (top %v)", got.Say, got.Short, got.Top)
	}
}

// Off-air captures, labelled by frequency (ubersdr-classifier's
// data/arecibo/<session>), give what the classifier repository's live test
// recorded for them. Set AIRSPLICE_SIGNAL_CAPTURES to such a directory.
func TestIdentifyCaptures(t *testing.T) {
	dir := os.Getenv("AIRSPLICE_SIGNAL_CAPTURES")
	if dir == "" {
		t.Skip("set AIRSPLICE_SIGNAL_CAPTURES to a directory of labelled captures (<label>_<kHz>_<time>.wav)")
	}
	eachSignalModel(t, func(t *testing.T, m *eatModel, _ *signalRefs) { identifyCaptures(t, m, dir) })
}

func identifyCaptures(t *testing.T, m *eatModel, dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	sort.Strings(files)
	if len(files) == 0 {
		t.Skip("no captures")
	}
	// FT8 and CW are what the badge names on air: in ubersdr-classifier's live
	// test, mn10 named FT8 10/10 and CW 7/8 and mn05 FT8 8/10 and CW 8/8, the
	// rest unnamed, never named as something else. Hold each model to that: of
	// the FT8 and CW captures together, at least signalFloor named right, and
	// none named wrong. (Per signal it is too few captures to hold to a rate.)
	expect := map[string]string{"ft8": "FT8", "cw": "CW"}
	const signalFloor = 0.8
	right, total := map[string]int{}, map[string]int{}
	for _, f := range files {
		label := strings.SplitN(filepath.Base(f), "_", 2)[0]
		sig, note := identifySegment(m, m.name, f)
		if sig == nil {
			t.Errorf("%s: %s", filepath.Base(f), note)
			continue
		}
		said := sig.Say
		switch sig.Say {
		case "name":
			said += ": " + sig.Short
		case "family":
			said += ": " + sig.FamilyText
		}
		var top []string
		for _, s := range sig.Top {
			top = append(top, fmt.Sprintf("%s %.2f", s.Name, s.P))
		}
		t.Logf("%-7s %-46s [%-16s] windows %d speech %d | %s", label, filepath.Base(f), said, sig.Windows, sig.Speech, strings.Join(top, ", "))
		want, ok := expect[label]
		if !ok {
			continue
		}
		total[label]++
		switch {
		case sig.Say == "name" && sig.Short == want:
			right[label]++
		case sig.Say == "name" || sig.Say == "family" && sig.FamilyText != "":
			t.Errorf("%s: want %s, said %s", filepath.Base(f), want, said)
		default:
			t.Logf("%s: %s not named (%s)", filepath.Base(f), want, said)
		}
	}
	var r, n int
	for label := range total {
		r += right[label]
		n += total[label]
	}
	t.Logf("FT8 %d/%d, CW %d/%d named right", right["ft8"], total["ft8"], right["cw"], total["cw"])
	if float64(r) < signalFloor*float64(n) {
		t.Errorf("FT8 and CW named %d of %d, under %.0f%%", r, n, signalFloor*100)
	}
}

// The build carries references for both models that identify: mn10 (the
// default) and mn05, each for its own weights and embedding width.
func TestShippedSignalModels(t *testing.T) {
	for model, dims := range map[string]int{"mn10": 1280, "mn05": 640} {
		if !hasSignals(model) {
			t.Errorf("signals_%s.bin is missing from the build", model)
			continue
		}
		ref, err := builtinSignals(model)
		if err != nil {
			t.Errorf("%s: %v", model, err)
			continue
		}
		if ref.d != dims {
			t.Errorf("%s references have %d dimensions, want %d", model, ref.d, dims)
		}
	}
}

// References made with other weights are refused.
func TestParseSignalsWrongModel(t *testing.T) {
	if _, err := parseSignals(signalsFile("mn05"), "mn10_as_mAP_471"); err == nil {
		t.Fatal("accepted mn05 references for mn10")
	}
	if _, err := builtinSignals("mn20"); err == nil && !hasSignals("mn20") {
		t.Fatal("references for a model the build has none for")
	}
}

func TestCombineRuns(t *testing.T) {
	named := func(n string, p float64) *signalResult {
		return &signalResult{Say: "name", Name: n, Short: n, P: p, Windows: 24}
	}
	fam := &signalResult{Say: "family", FamilyText: "FSK", FamilyP: 0.8, Windows: 24}
	voice := &signalResult{Say: "voice", Windows: 24, Speech: 10}
	none := &signalResult{Say: "nothing", Name: "X", P: 0.3, Windows: 24}
	starts := []float64{0, 60, 120, 180}
	cases := []struct {
		in       []*signalResult
		say, txt string
	}{
		{[]*signalResult{named("CW", 0.8), named("FT8", 0.75), named("FT8", 0.9)}, "name", "FT8"},
		{[]*signalResult{named("CW", 0.95), named("FT8", 0.75)}, "name", "CW"}, // a tie: the surer
		{[]*signalResult{voice, fam, voice}, "family", "FSK"},
		{[]*signalResult{voice, voice, none}, "voice", ""},
		{[]*signalResult{voice, none, none}, "nothing", ""},
	}
	for i, c := range cases {
		got := combineRuns(c.in, starts[:len(c.in)])
		txt := got.Short
		if got.Say == "family" {
			txt = got.FamilyText
		}
		if got.Say == "voice" || got.Say == "nothing" {
			txt = ""
		}
		if got.Say != c.say || txt != c.txt || len(got.Runs) != len(c.in) || got.Windows != 24*len(c.in) {
			t.Errorf("case %d: got %s %q, %d runs, %d windows", i, got.Say, txt, len(got.Runs), got.Windows)
		}
	}
}

// A segment of CW then FT8 is scored a minute at a time: the minutes name each,
// the segment is called after the one most minutes name, and both are counted.
func TestIdentifyMixedSegment(t *testing.T) {
	dir := os.Getenv("AIRSPLICE_SIGNAL_CAPTURES")
	if dir == "" {
		t.Skip("set AIRSPLICE_SIGNAL_CAPTURES (see TestIdentifyCaptures)")
	}
	eachSignalModel(t, func(t *testing.T, m *eatModel, ref *signalRefs) { identifyMixed(t, m, ref, dir) })
}

func identifyMixed(t *testing.T, m *eatModel, ref *signalRefs, dir string) {
	read := func(name string) ([]float32, int) {
		w, err := openWAVStream(filepath.Join(dir, name))
		if err != nil {
			t.Skip(err)
		}
		defer w.Close()
		var x []float32
		for {
			b, err := w.read(1 << 16)
			x = append(x, b...)
			if err != nil {
				return x, w.rate
			}
		}
	}
	cw, r1 := read("cw_14041_20261116T194323Z.wav")
	ft8, r2 := read("ft8_10136_20261116T192953Z.wav")
	ft8b, r3 := read("ft8_3573_20261116T193056Z.wav")
	if r1 != r2 || r2 != r3 {
		t.Skip("captures at different rates")
	}
	x := append(append(append([]float32{}, cw...), ft8...), ft8b...)
	got, err := m.identifyFrom(newResampledReader(&sliceSource{x: x}, r1, eatRate), ref)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, r := range got.Runs {
		calls = append(calls, fmt.Sprintf("%.0fs:%s %s", r.StartSec, r.Say, r.Short))
	}
	t.Logf("segment: %s %s; runs %v; counts %v", got.Say, got.Short, calls, got.counts())
	if got.Say != "name" || got.Short != "FT8" || len(got.Runs) != 3 || got.Runs[0].Short != "CW" {
		t.Fatalf("got %s %q with runs %v", got.Say, got.Short, calls)
	}
	if c := got.counts(); len(c) != 2 {
		t.Fatalf("counts %v, want CW and FT8", c)
	}
}
