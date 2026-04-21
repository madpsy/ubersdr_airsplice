// fft.go — Pure-Go radix-2 FFT + audioFFT accumulator for the audio panel.
// Ported from ubersdr_qsstv with identical constants so the frontend
// spectrum/waterfall display is compatible.
package main

import (
	"encoding/binary"
	"math"
	"math/cmplx"
)

// ---------------------------------------------------------------------------
// Radix-2 Cooley-Tukey FFT (pure Go, no cgo)
// ---------------------------------------------------------------------------

// fftRadix2 computes the in-place DIT FFT of x (length must be a power of 2).
func fftRadix2(x []complex128) {
	n := len(x)
	if n <= 1 {
		return
	}
	// Bit-reversal permutation
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	// Butterfly stages
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		wlen := complex(math.Cos(angle), math.Sin(angle))
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			half := length / 2
			for k := 0; k < half; k++ {
				u := x[i+k]
				v := x[i+k+half] * w
				x[i+k] = u + v
				x[i+k+half] = u - v
				w *= wlen
			}
		}
	}
}

// ---------------------------------------------------------------------------
// audioFFT — accumulates PCM samples and emits magnitude spectra
// ---------------------------------------------------------------------------

const (
	fftSize   = 2048 // must be power of 2; gives ~5 Hz resolution at 11025 Hz
	fftLowHz  = 200  // display range low (Hz)
	fftHighHz = 2900 // display range high (Hz)
	fftBins   = 256  // number of output bins (downsampled from fftSize/2)
)

// fftMagnitudes holds one spectrum frame: fftBins magnitude values in dBFS,
// plus the current volume level in dBFS.
type fftMagnitudes struct {
	Bins       []float32 `json:"bins"`
	VolumeDB   float32   `json:"volume_db"`
	SampleRate int       `json:"sample_rate"`
}

type audioFFT struct {
	buf        []float64    // ring buffer of PCM samples
	bufPos     int
	sampleRate int
	window     []float64    // Hann window coefficients
	cx         []complex128
	// exponential averaging state
	avgBins  []float64
	avgAlpha float64 // fast attack
	avgDecay float64 // slow decay
}

func newAudioFFT(sampleRate int) *audioFFT {
	a := &audioFFT{
		buf:        make([]float64, fftSize),
		sampleRate: sampleRate,
		window:     make([]float64, fftSize),
		cx:         make([]complex128, fftSize),
		avgBins:    make([]float64, fftSize/2),
		avgAlpha:   0.4,  // fast attack (matches QSSTV)
		avgDecay:   0.10, // slow decay
	}
	// Pre-compute Hann window
	for i := 0; i < fftSize; i++ {
		a.window[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(fftSize-1)))
	}
	// Init avg to silence
	for i := range a.avgBins {
		a.avgBins[i] = -100
	}
	return a
}

// push adds S16LE PCM bytes (mono) to the ring buffer.
// Returns a fftMagnitudes frame each time a full fftSize window is ready.
// May return nil if not enough samples yet.
func (a *audioFFT) push(pcm []byte) *fftMagnitudes {
	n := len(pcm) / 2
	for i := 0; i < n; i++ {
		s := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		a.buf[a.bufPos] = float64(s) / 32768.0
		a.bufPos++
		if a.bufPos >= fftSize {
			a.bufPos = 0
			return a.compute()
		}
	}
	return nil
}

// compute runs the FFT on the current buffer and returns magnitude bins.
func (a *audioFFT) compute() *fftMagnitudes {
	// Apply Hann window and fill complex input
	for i := 0; i < fftSize; i++ {
		v := a.buf[i] * a.window[i]
		a.cx[i] = complex(v, 0)
	}

	fftRadix2(a.cx)

	// Compute RMS volume
	var rms float64
	for i := 0; i < fftSize; i++ {
		rms += a.buf[i] * a.buf[i]
	}
	rms = math.Sqrt(rms / float64(fftSize))
	var volDB float32
	if rms > 1e-10 {
		volDB = float32(20 * math.Log10(rms))
	} else {
		volDB = -100
	}

	// Map FFT bins to display range [fftLowHz, fftHighHz]
	step := float64(a.sampleRate) / float64(fftSize)
	binBegin := int(math.Round(float64(fftLowHz) / step))
	binEnd := int(math.Round(float64(fftHighHz) / step))
	if binEnd > fftSize/2 {
		binEnd = fftSize / 2
	}
	binSpan := binEnd - binBegin
	if binSpan <= 0 {
		binSpan = 1
	}

	// Downsample to fftBins output bins with exponential averaging
	out := make([]float32, fftBins)
	for j := 0; j < fftBins; j++ {
		srcStart := binBegin + j*binSpan/fftBins
		srcEnd := binBegin + (j+1)*binSpan/fftBins
		if srcEnd <= srcStart {
			srcEnd = srcStart + 1
		}
		if srcEnd > fftSize/2 {
			srcEnd = fftSize / 2
		}
		var power float64
		for k := srcStart; k < srcEnd; k++ {
			mag := cmplx.Abs(a.cx[k]) / float64(fftSize)
			power += mag * mag
		}
		power /= float64(srcEnd - srcStart)

		var db float64
		if power > 1e-20 {
			db = 10 * math.Log10(power)
		} else {
			db = -100
		}

		// Asymmetric exponential average: fast attack, slow decay
		prev := a.avgBins[j]
		if db > prev {
			a.avgBins[j] = prev*(1-a.avgAlpha) + a.avgAlpha*db
		} else {
			a.avgBins[j] = prev*(1-a.avgDecay) + a.avgDecay*db
		}
		out[j] = float32(a.avgBins[j])
	}

	return &fftMagnitudes{
		Bins:       out,
		VolumeDB:   volDB,
		SampleRate: a.sampleRate,
	}
}
