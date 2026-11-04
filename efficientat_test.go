package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"

	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// testdata/tiny_state_dict.pt was written by torch.save from a known state
// dict (see the values below), including an integer num_batches_tracked.
func TestReadPtStateDict(t *testing.T) {
	sd, err := readPtStateDict("testdata/tiny_state_dict.pt")
	if err != nil {
		t.Fatal(err)
	}
	w := sd["layer.weight"]
	if len(w.Shape) != 2 || w.Shape[0] != 2 || w.Shape[1] != 3 {
		t.Fatalf("layer.weight shape %v", w.Shape)
	}
	for i, v := range w.Data { // arange(6)/4 - 0.5
		if v != float32(i)/4-0.5 {
			t.Fatalf("layer.weight[%d] = %v", i, v)
		}
	}
	if b := sd["layer.bias"].Data; len(b) != 2 || b[0] != 1.5 || b[1] != -2.25 {
		t.Fatalf("layer.bias %v", b)
	}
	if s := sd["scalar"]; len(s.Shape) != 0 || len(s.Data) != 1 || s.Data[0] != 3 {
		t.Fatalf("scalar %+v", s)
	}
	big := sd["big.weight"]
	if big.numel() != 300 || big.Data[0] != -1 || big.Data[299] != 1 {
		t.Fatalf("big.weight %v ... %v", big.Data[0], big.Data[299])
	}
	if _, ok := sd["bn.num_batches_tracked"]; ok {
		t.Fatal("an integer tensor was returned as float")
	}
}

// Every byte of the weights file comes off the network: corruption must be an
// error, never a panic.
func TestReadPtRejectsCorruption(t *testing.T) {
	zr, err := zip.OpenReader("testdata/tiny_state_dict.pt")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	var names []string
	for _, f := range zr.File {
		b, _ := readZipFile(f)
		entries[f.Name] = b
		names = append(names, f.Name)
	}
	zr.Close()
	dir := t.TempDir()
	r := rand.New(rand.NewSource(1))
	write := func(mutate func(name string, b []byte) []byte) string {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, n := range names {
			w, _ := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Store})
			_, _ = w.Write(mutate(n, append([]byte(nil), entries[n]...)))
		}
		zw.Close()
		p := filepath.Join(dir, "x.pt")
		_ = os.WriteFile(p, buf.Bytes(), 0o644)
		return p
	}
	for i := 0; i < 400; i++ {
		p := write(func(name string, b []byte) []byte {
			if filepath.Base(name) != "data.pkl" || len(b) == 0 {
				return b
			}
			if i%4 == 0 {
				return b[:r.Intn(len(b))] // truncated
			}
			for k := 0; k < 3; k++ {
				b[r.Intn(len(b))] = byte(r.Intn(256))
			}
			return b
		})
		_, _ = readPtStateDict(p) // must not panic
	}
	garbage := filepath.Join(dir, "garbage.pt")
	_ = os.WriteFile(garbage, []byte("definitely not a zip"), 0o644)
	if _, err := readPtStateDict(garbage); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestMakeDivisible(t *testing.T) {
	for _, c := range []struct {
		v    float64
		want int
	}{{8, 8}, {36, 40}, {640, 640}, {1280, 1280}, {2560, 2560}, {120, 120}, {4, 8}, {60, 64}} {
		if got := makeDivisible(c.v, 8); got != c.want {
			t.Errorf("makeDivisible(%v) = %d, want %d", c.v, got, c.want)
		}
	}
}

func probsWith(vals map[string]float32) []float32 {
	labels := audiosetLabelNames()
	p := make([]float32, len(labels))
	for i, l := range labels {
		if v, ok := vals[l]; ok {
			p[i] = v
		} else {
			p[i] = 0.001
		}
	}
	return p
}

// NightDX's fold of AudioSet labels into voice / music / noise.
func TestEATWindowVerdict(t *testing.T) {
	if n := len(audiosetLabelNames()); n != 527 {
		t.Fatalf("%d AudioSet labels", n)
	}
	cases := []struct {
		probs map[string]float32
		want  string
	}{
		{map[string]float32{"Speech": 0.9, "Radio": 0.6}, "voice"},
		{map[string]float32{"Music": 0.8, "Guitar": 0.3}, "music"},
		{map[string]float32{"Speech": 0.3, "Music": 0.5}, "music"},
		{map[string]float32{"Speech": 0.08, "Music": 0.05}, "noise"},                    // neither reaches 0.12
		{map[string]float32{"Static": 0.6, "White noise": 0.5, "Speech": 0.3}, "noise"}, // noise > 1.5x voice
		{map[string]float32{"Hum": 0.9, "Speech": 0.4}, "voice"},                        // hum rides on the programme
	}
	for _, c := range cases {
		if got, _, _ := eatWindowVerdict(probsWith(c.probs)); got != c.want {
			t.Errorf("%v: %s, want %s", c.probs, got, c.want)
		}
	}
	if !radioTag("Male speech, man speaking") || !radioTag("Mains hum") || radioTag("Vehicle") || radioTag("Insect") {
		t.Error("radio tag filter")
	}
}

// eatModelDir is where the test looks for the downloaded .pt files; the test
// is skipped without them (they are 6-72 MB and fetched from GitHub).
func eatModelDir(t *testing.T) string {
	dir := os.Getenv("AIRSPLICE_EAT_MODELS")
	if dir == "" {
		t.Skip("set AIRSPLICE_EAT_MODELS to a directory holding the EfficientAT .pt files")
	}
	return dir
}

// eatGoldenInput reproduces the input the golden outputs were computed from:
// the synthetic signals of analysis_test.go at 16 kHz, stored as a 16-bit WAV,
// resampled to 32 kHz, the first 10 s, stored as a 16-bit WAV again. A WAV
// round trip is floatToInt16 (x32767) on the way out and /32768 on the way in.
func eatGoldenInput(t *testing.T, name string) []float32 {
	var x []float32
	switch name {
	case "chords":
		x = chords(analysisRate, 20)
	case "noise":
		x = whiteNoise(analysisRate*20, 0.1, 1)
	}
	wavRoundTrip := func(v []float32) {
		for i, s := range v {
			v[i] = float32(floatToInt16(s)) / 32768
		}
	}
	wavRoundTrip(x)
	y := resample(x, analysisRate, eatRate)[:10*eatRate]
	wavRoundTrip(y)
	return y
}

// Golden outputs from an independent PyTorch implementation (torch.stft,
// torchaudio's Kaldi mel banks, F.conv2d / batch_norm / linear) on the
// official weights: testdata/efficientat_golden.json.
func TestEfficientATMatchesReference(t *testing.T) {
	dir := eatModelDir(t)
	raw, err := os.ReadFile("testdata/efficientat_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	for _, sig := range []string{"chords", "noise"} {
		spec := eatMelSpec(eatGoldenInput(t, sig))
		var shape []int
		var sum float64
		_ = json.Unmarshal(golden[sig+"/mel_shape"], &shape)
		_ = json.Unmarshal(golden[sig+"/mel_sum"], &sum)
		if spec.h != shape[0] || spec.w != shape[1] {
			t.Fatalf("%s mel %dx%d, reference %v", sig, spec.h, spec.w, shape)
		}
		var got float64
		for _, v := range spec.d {
			got += float64(v)
		}
		if math.Abs(got-sum) > 1e-4*math.Abs(sum)+0.1 {
			t.Errorf("%s mel sum %.3f, reference %.3f", sig, got, sum)
		}
		for _, name := range []string{"mn05", "mn10", "mn20"} {
			info := eatModels[name]
			p := filepath.Join(dir, info.pin.File)
			if _, err := os.Stat(p); err != nil {
				t.Logf("%s: %v (skipped)", name, err)
				continue
			}
			m, err := loadEfficientAT(p, name, info.width, 2)
			if err != nil {
				t.Fatal(err)
			}
			probs := m.predict(spec)
			var top [][2]float64
			_ = json.Unmarshal(golden[sig+"/"+name], &top)
			for _, e := range top {
				i, want := int(e[0]), e[1]
				// 5e-4: on amd64 the outputs agree to ~1e-5; on arm64 Go fuses
				// multiply-adds even in plain Go code, and the deepest model
				// (mn20) carries that to ~2e-4 on white noise. An error in the
				// port moves outputs by far more than this.
				if d := math.Abs(float64(probs[i]) - want); d > 5e-4 {
					t.Errorf("%s %s class %d (%s): %.6f, reference %.6f", sig, name, i, audiosetLabelNames()[i], probs[i], want)
				}
			}
		}
	}
}

// The SIMD kernels and the pure Go loops give the same model output.
func TestEfficientATSIMDMatchesGo(t *testing.T) {
	dir := eatModelDir(t)
	if !f32SIMD {
		t.Skip("no vector kernels here")
	}
	info := eatModels["mn05"]
	m, err := loadEfficientAT(filepath.Join(dir, info.pin.File), "mn05", info.width, 1)
	if err != nil {
		t.Skip(err)
	}
	spec := eatMelSpec(eatGoldenInput(t, "chords"))
	a := m.predict(spec)
	var b []float32
	withF32SIMD(false, func() { b = m.predict(spec) })
	for i := range a {
		if math.Abs(float64(a[i]-b[i])) > 1e-5 {
			t.Fatalf("class %d: SIMD %v, Go %v", i, a[i], b[i])
		}
	}
}

func BenchmarkEfficientATmn10(b *testing.B) {
	dir := os.Getenv("AIRSPLICE_EAT_MODELS")
	if dir == "" {
		b.Skip("set AIRSPLICE_EAT_MODELS")
	}
	m, err := loadEfficientAT(filepath.Join(dir, eatModels["mn10"].pin.File), "mn10", 1, 1)
	if err != nil {
		b.Fatal(err)
	}
	spec := eatMelSpec(sine(eatRate, 440, 0.3, 10))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.predict(spec)
	}
}
