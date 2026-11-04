// analysis_audio.go — reading segments back and resampling them for analysis.
package main

import (
	"io"
	"math"
)

// analysisRate is what the classifier and Whisper work at. UberSDR sends
// 12 kHz for SSB/CW and 24 kHz for AM/FM, so segments are resampled to it.
const analysisRate = 16000

// readWAVMono reads a whole 16-bit PCM WAV file as float32 in -1..1, mixed
// to mono. Analysis streams segments instead (openWAVStream); this is for
// small files and the tests.
func readWAVMono(path string) ([]float32, int, error) {
	w, err := openWAVStream(path)
	if err != nil {
		return nil, 0, err
	}
	defer w.Close()
	var out []float32
	for {
		x, err := w.read(1 << 16)
		out = append(out, x...)
		if err == io.EOF {
			return out, w.rate, nil
		}
		if err != nil {
			return nil, 0, err
		}
	}
}

// resample converts x from one rate to another with a windowed-sinc filter
// (Blackman window, 16 zero crossings either side at the lower of the two
// Nyquist frequencies). Quality is ample for classification and speech
// recognition; it is not a mastering resampler. It is the stream resampler
// (analysis_stream.go) fed everything at once.
func resample(x []float32, from, to int) []float32 {
	if from == to || len(x) == 0 {
		return x
	}
	r := newStreamResampler(from, to)
	out := r.push(x)
	return append(out, r.flush()...)
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
