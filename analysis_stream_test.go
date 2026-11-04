package main

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// resampleDirect is the original whole-array resampler, kept here as an
// independent reference for the streaming one.
func resampleDirect(x []float32, from, to int) []float32 {
	if from == to || len(x) == 0 {
		return x
	}
	g := gcd(from, to)
	up, down := to/g, from/g
	r := math.Min(1, float64(to)/float64(from)) * 0.95
	half := int(math.Ceil(16 / r))
	taps := 2*half + 1
	table := make([][]float64, up)
	for p := 0; p < up; p++ {
		frac := float64(p) / float64(up)
		h := make([]float64, taps)
		var sum float64
		for k := 0; k < taps; k++ {
			t := float64(k-half) - frac
			h[k] = r * sinc(r*t) * blackman(t, float64(half)+1)
			sum += h[k]
		}
		for k := range h {
			h[k] /= sum
		}
		table[p] = h
	}
	n := len(x) * up / down
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		pos := i * down
		base, h := pos/up, table[pos%up]
		var acc float64
		for k := 0; k < taps; k++ {
			if j := base - half + k; j >= 0 && j < len(x) {
				acc += float64(float32(h[k])) * float64(x[j])
			}
		}
		out[i] = float32(acc)
	}
	return out
}

func TestStreamResamplerMatchesWholeArray(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	x := randF32(r, 24000*3+137)
	for _, rates := range [][2]int{{24000, 16000}, {12000, 16000}, {24000, 32000}, {12000, 32000}, {48000, 16000}} {
		whole := resample(x, rates[0], rates[1])
		ref := resampleDirect(x, rates[0], rates[1])
		if len(whole) != len(ref) {
			t.Fatalf("%v: %d samples, reference %d", rates, len(whole), len(ref))
		}
		for i := range ref {
			if math.Abs(float64(whole[i]-ref[i])) > 1e-5 {
				t.Fatalf("%v [%d]: %v, reference %v", rates, i, whole[i], ref[i])
			}
		}
		// Fed in random pieces, the stream gives exactly the same samples.
		s := newStreamResampler(rates[0], rates[1])
		var got []float32
		for i := 0; i < len(x); {
			n := 1 + r.Intn(3000)
			if i+n > len(x) {
				n = len(x) - i
			}
			got = append(got, s.push(x[i:i+n])...)
			i += n
		}
		got = append(got, s.flush()...)
		if len(got) != len(whole) {
			t.Fatalf("%v: streamed %d samples, whole %d", rates, len(got), len(whole))
		}
		for i := range got {
			if got[i] != whole[i] {
				t.Fatalf("%v [%d]: streamed %v, whole %v", rates, i, got[i], whole[i])
			}
		}
	}
}

// forEachWindow reproduces NightDX's windowing loop exactly.
func TestForEachWindowMatchesNightDX(t *testing.T) {
	const win, minLen = 100, 20
	for n := 0; n <= 430; n++ {
		var want [][2]int // NightDX: for i in range(0, max(n - win//2, 1), win)
		for i := 0; i < max(n-win/2, 1); i += win {
			end := min(i+win, n)
			if end-i >= minLen {
				want = append(want, [2]int{i, end})
			}
		}
		x := make([]float32, n)
		for i := range x {
			x[i] = float32(i)
		}
		var got [][2]int
		_ = forEachWindow(newResampledReader(&sliceSource{x: x}, 1000, 1000), win, minLen, func(w []float32) {
			got = append(got, [2]int{int(w[0]), int(w[0]) + len(w)})
		})
		if len(got) != len(want) {
			t.Fatalf("n=%d: windows %v, want %v", n, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("n=%d: windows %v, want %v", n, got, want)
			}
		}
	}
}

func TestWAVStreamReadsFinalisedAndOrphaned(t *testing.T) {
	dir := t.TempDir()
	x := sine(12000, 300, 0.4, 2.5)
	for _, finalise := range []bool{true, false} {
		p := filepath.Join(dir, "s.wav")
		writeTestWAV(t, p, 12000, x, finalise)
		w, err := openWAVStream(p)
		if err != nil {
			t.Fatal(err)
		}
		var got []float32
		for {
			c, err := w.read(997)
			got = append(got, c...)
			if err != nil {
				break
			}
		}
		w.Close()
		if w.rate != 12000 || len(got) != len(x) {
			t.Fatalf("finalise=%v: rate %d, %d samples, want %d", finalise, w.rate, len(got), len(x))
		}
	}
	// An extra chunk before the data is skipped.
	p := filepath.Join(dir, "list.wav")
	f, _ := os.Create(p)
	data := make([]byte, 2*len(x))
	for i, v := range x {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(floatToInt16(v)))
	}
	put := func(s string) { f.Write([]byte(s)) }
	u32 := func(v uint32) { var b [4]byte; binary.LittleEndian.PutUint32(b[:], v); f.Write(b[:]) }
	u16 := func(v uint16) { var b [2]byte; binary.LittleEndian.PutUint16(b[:], v); f.Write(b[:]) }
	put("RIFF")
	u32(uint32(4 + 24 + 13 + 1 + 8 + len(data)))
	put("WAVE")
	put("fmt ")
	u32(16)
	u16(1)
	u16(1)
	u32(12000)
	u32(24000)
	u16(2)
	u16(16)
	put("LIST")
	u32(5)
	put("hello")
	f.Write([]byte{0}) // odd size, padded
	put("data")
	u32(uint32(len(data)))
	f.Write(data)
	f.Close()
	y, rate, err := readWAVMono(p)
	if err != nil || rate != 12000 || len(y) != len(x) {
		t.Fatalf("LIST chunk: %v rate %d len %d", err, rate, len(y))
	}
}

// A segment classified from its file gives the same result as from memory.
func TestStreamedClassificationMatchesInMemory(t *testing.T) {
	x := chords(24000, 23)
	p := filepath.Join(t.TempDir(), "c.wav")
	writeTestWAV(t, p, 24000, x, true)
	mem, _, _ := readWAVMono(p)
	want := classifyDSP(resample(mem, 24000, analysisRate), analysisRate, dspWindowSec, 250, 3500)
	w, err := openWAVStream(p)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	got, err := classifyDSPFrom(newResampledReader(w, w.rate, analysisRate), analysisRate, dspWindowSec, 250, 3500)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != want.Label || len(got.Windows) != len(want.Windows) {
		t.Fatalf("streamed %s %v, in memory %s %v", got.Label, got.Windows, want.Label, want.Windows)
	}
	for i := range got.Windows {
		if got.Windows[i] != want.Windows[i] {
			t.Fatalf("window %d: streamed %s, in memory %s", i, got.Windows[i], want.Windows[i])
		}
	}
}
