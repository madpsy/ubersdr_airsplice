// analysis_audio.go — reading segments back and resampling them for analysis.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// analysisRate is what the classifier and Whisper work at. UberSDR sends
// 12 kHz for SSB/CW and 24 kHz for AM/FM, so segments are resampled to it.
const analysisRate = 16000

// readWAVMono reads a 16-bit PCM WAV file as float32 in -1..1, mixing to mono.
// A data chunk whose size was never finalised (the process died mid-segment)
// is read to the end of the file.
func readWAVMono(path string) ([]float32, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, 0, fmt.Errorf("wav: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return nil, 0, errors.New("wav: not a RIFF/WAVE file")
	}
	var (
		rate, channels, bits int
		haveFmt              bool
	)
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return nil, 0, errors.New("wav: no data chunk")
		}
		id := string(ch[0:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch id {
		case "fmt ":
			b := make([]byte, size)
			if _, err := io.ReadFull(f, b); err != nil || size < 16 {
				return nil, 0, errors.New("wav: bad fmt chunk")
			}
			if format := binary.LittleEndian.Uint16(b[0:2]); format != 1 {
				return nil, 0, fmt.Errorf("wav: format %d is not PCM", format)
			}
			channels = int(binary.LittleEndian.Uint16(b[2:4]))
			rate = int(binary.LittleEndian.Uint32(b[4:8]))
			bits = int(binary.LittleEndian.Uint16(b[14:16]))
			haveFmt = true
			if size%2 == 1 {
				_, _ = f.Seek(1, io.SeekCurrent)
			}
		case "data":
			if !haveFmt {
				return nil, 0, errors.New("wav: data before fmt")
			}
			if bits != 16 || channels < 1 || rate <= 0 {
				return nil, 0, fmt.Errorf("wav: unsupported %d-bit %d-channel %d Hz", bits, channels, rate)
			}
			var data []byte
			if size == 0 || size == 0xFFFFFFFF {
				data, err = io.ReadAll(f)
			} else {
				data = make([]byte, size)
				var n int
				n, err = io.ReadFull(f, data)
				data = data[:n]
				if err == io.ErrUnexpectedEOF {
					err = nil
				}
			}
			if err != nil {
				return nil, 0, fmt.Errorf("wav: %w", err)
			}
			frames := len(data) / (2 * channels)
			out := make([]float32, frames)
			for i := 0; i < frames; i++ {
				var acc int32
				for c := 0; c < channels; c++ {
					acc += int32(int16(binary.LittleEndian.Uint16(data[(i*channels+c)*2:])))
				}
				out[i] = float32(acc) / float32(channels) / 32768
			}
			return out, rate, nil
		default:
			if _, err := f.Seek(size+size%2, io.SeekCurrent); err != nil {
				return nil, 0, fmt.Errorf("wav: %w", err)
			}
		}
	}
}

// resample converts x from one rate to another with a windowed-sinc filter
// (Blackman window, 16 zero crossings either side at the lower of the two
// Nyquist frequencies). Quality is ample for classification and speech
// recognition; it is not a mastering resampler.
func resample(x []float32, from, to int) []float32 {
	if from == to || len(x) == 0 {
		return x
	}
	g := gcd(from, to)
	up, down := to/g, from/g // output sample n sits at input position n*down/up

	r := math.Min(1, float64(to)/float64(from)) * 0.95 // cutoff, relative to input Nyquist, with margin
	const zeros = 16
	half := int(math.Ceil(zeros / r)) // taps either side, in input samples
	taps := 2*half + 1

	// One filter per fractional phase: output n's position has fractional part
	// (n*down mod up)/up, so there are `up` distinct phases.
	table := make([][]float32, up)
	for p := 0; p < up; p++ {
		frac := float64(p) / float64(up)
		h := make([]float32, taps)
		var sum float64
		for k := 0; k < taps; k++ {
			t := float64(k-half) - frac // distance from the output position, input samples
			v := r * sinc(r*t) * blackman(t, float64(half)+1)
			h[k] = float32(v)
			sum += v
		}
		for k := range h { // unity gain at DC
			h[k] = float32(float64(h[k]) / sum)
		}
		table[p] = h
	}

	n := int(int64(len(x)) * int64(up) / int64(down))
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		pos := int64(i) * int64(down)
		base := int(pos / int64(up))
		h := table[int(pos%int64(up))]
		var acc float32
		start := base - half
		for k, hk := range h {
			j := start + k
			if j >= 0 && j < len(x) {
				acc += hk * x[j]
			}
		}
		out[i] = acc
	}
	return out
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// blackman is the Blackman window over [-width, width], zero outside.
func blackman(t, width float64) float64 {
	if t <= -width || t >= width {
		return 0
	}
	u := (t + width) / (2 * width)
	return 0.42 - 0.5*math.Cos(2*math.Pi*u) + 0.08*math.Cos(4*math.Pi*u)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
