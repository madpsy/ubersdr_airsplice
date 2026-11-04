// classify_dsp.go — voice / music / noise without a model.
//
// A port of NightDX's DSP classifier (nightdx/classify.py, classify_dsp and
// _fold; github.com/marcozennaro/nightdx, MIT, Copyright (c) 2026 Marco
// Zennaro). The thresholds are NightDX's, unchanged. Each window of audio is
// measured three ways over the 250-3500 Hz band:
//
//   - spectral flatness: how close the spectrum is to white (1 = white noise);
//   - syllabic rhythm: the share of the loudness envelope's modulation that
//     falls at 2-8 Hz, the rate of syllables in speech;
//   - steadiness: how closely each short-time spectrum matches the one before,
//     which held musical notes make high.
//
// One change from NightDX: the analysis band is capped at the channel's audio
// passband (analysisBand). NightDX listened to AM broadcasts through a 4.5 kHz
// audio filter, so 250-3500 Hz sat inside it. An SSB channel's 2.7 kHz filter
// edge falls inside that band, and the fixed shape of the filter skirt then
// dominates every short-time spectrum: successive frames correlate at ~0.92
// and SSB voice reads as music. With the band capped at the passband, the same
// recordings measured 0.21-0.33 and classify as voice, while FT8 (steady
// tones) stays at 0.76-0.88. Measured on a handful of this receiver's
// recordings (two of SSB voice, three of FT8), so the cap fixes a clear
// defect, but the thresholds have not been re-tuned on a large set.
//
// The STFT and Welch estimate follow scipy's defaults (periodic Hann, half
// overlap, zero-padded boundaries, per-segment mean removal). Every quantity is
// a ratio, so their scaling conventions do not matter.
package main

import (
	"math"
	"sort"
)

const (
	dspWindowSec = 5.0
	dspNperseg   = 512
	dspHop       = 256
)

// analysisBand is the audio band, in Hz, that the classifier measures for a
// channel: NightDX's 250-3500 Hz, capped at 90% of the top of the channel's
// audio passband. bwHz is the channel's filter bandwidth (0 = server default).
func analysisBand(mode string, bwHz int) (lo, hi float64) {
	lo, hi = 250, 3500
	var top float64
	switch mode {
	case "usb", "lsb":
		lo = 300 // airsplice's SSB passband starts at 300 Hz (bandwidthParams)
		top = 2700
		if bwHz > 0 {
			top = float64(bwHz)
		}
	case "am", "sam":
		top = 2500 // half of the 5 kHz default
		if bwHz > 0 {
			top = float64(bwHz) / 2
		}
	case "fm", "nfm":
		top = 4000
		if bwHz > 0 {
			top = float64(bwHz) / 2
		}
	default: // cw and anything else: NightDX's band
		return lo, hi
	}
	if c := 0.9 * top; c < hi {
		hi = c
	}
	if hi < lo+500 { // a very narrow filter: keep a usable band
		hi = lo + 500
	}
	return lo, hi
}

func classifyDSP(audio []float32, rate int, windowSec float64, lo, hi float64) *classificationResult {
	res, _ := classifyDSPFrom(newResampledReader(&sliceSource{x: audio}, rate, rate), rate, windowSec, lo, hi)
	return res
}

// classifyDSPFrom classifies a stream already at `rate`, window by window, so
// memory does not grow with the segment.
func classifyDSPFrom(r *resampledReader, rate int, windowSec float64, lo, hi float64) (*classificationResult, error) {
	var windows []string
	err := forEachWindow(r, int(windowSec*float64(rate)), rate, func(x []float32) {
		windows = append(windows, dspVerdict(dspWindowMetrics(x, rate, lo, hi)))
	})
	if err != nil {
		return nil, err
	}
	res := foldWindows(windows, "dsp", 0.5)
	res.BandHz = []int{int(lo), int(hi)}
	return res, nil
}

// foldWindows turns per-window verdicts into the segment's label (NightDX _fold).
func foldWindows(windows []string, backend string, conf float64) *classificationResult {
	res := &classificationResult{Status: analysisDone, Backend: backend, Confidence: conf, Windows: windows}
	if len(windows) == 0 {
		res.Label, res.NoisePct = "noise", 100
		res.Reason = "too little audio to classify"
		return res
	}
	var v, m, z int
	for _, w := range windows {
		switch w {
		case "voice":
			v++
		case "music":
			m++
		default:
			z++
		}
	}
	n := float64(len(windows))
	res.VoicePct, res.MusicPct, res.NoisePct = 100*float64(v)/n, 100*float64(m)/n, 100*float64(z)/n
	switch {
	case res.NoisePct >= 60:
		res.Label = "noise"
	case res.VoicePct >= 25 && res.MusicPct >= 25:
		res.Label = "voice+music"
	case res.VoicePct >= res.MusicPct:
		res.Label = "voice"
	default:
		res.Label = "music"
	}
	return res
}

// dspMetrics are one window's measurements.
type dspMetrics struct {
	Flat, Syll, Corr float64 // median flatness, syllabic share, mean steadiness
	ok               bool    // false when the window was too short to measure
}

// dspVerdict applies NightDX's thresholds.
func dspVerdict(m dspMetrics) string {
	switch {
	case !m.ok:
		return "noise"
	case m.Flat > 0.55 && m.Corr < 0.35:
		return "noise"
	case m.Syll > 0.45 && m.Corr < 0.75:
		return "voice"
	case m.Corr >= 0.5:
		return "music"
	case m.Syll > 0.3:
		return "voice"
	default:
		return "noise"
	}
}

func dspWindowMetrics(x []float32, rate int, lo, hi float64) dspMetrics {
	P, nFrames := stftPower(x)
	// The band, as bin indices of a dspNperseg-point FFT.
	var band []int
	for k := 0; k <= dspNperseg/2; k++ {
		f := float64(k) * float64(rate) / dspNperseg
		if f > lo && f < hi {
			band = append(band, k)
		}
	}
	nb := float64(len(band))
	if nFrames < 2 || len(band) == 0 {
		return dspMetrics{}
	}

	flat := make([]float64, nFrames)
	env := make([]float64, nFrames)
	logP := make([][]float64, nFrames) // per frame, band only, mean-removed
	for t := 0; t < nFrames; t++ {
		var sumLog, sum float64
		lp := make([]float64, len(band))
		for i, k := range band {
			p := P[t][k]
			lp[i] = math.Log(p)
			sumLog += lp[i]
			sum += p
		}
		flat[t] = math.Exp(sumLog/nb) / (sum / nb)
		env[t] = sum
		mean := sumLog / nb
		for i := range lp {
			lp[i] -= mean
		}
		logP[t] = lp
	}

	// Syllabic rhythm: Welch spectrum of the normalised envelope.
	var envMean float64
	for _, e := range env {
		envMean += e
	}
	envMean /= float64(nFrames)
	for t := range env {
		env[t] = env[t]/(envMean+1e-12) - 1 // env/mean, then minus its mean (which is 1)
	}
	fsEnv := float64(rate) / dspHop
	fe, pe := welch(env, fsEnv, min(len(env), 128))
	var num, den float64
	for i, f := range fe {
		if f > 2 && f < 8 {
			num += pe[i]
		}
		if f > 0.5 && f < 20 {
			den += pe[i]
		}
	}
	syll := num / (den + 1e-12)

	// Steadiness: mean cosine similarity of successive log spectra.
	var corr float64
	for t := 1; t < nFrames; t++ {
		var dot, na, nb2 float64
		for i := range logP[t] {
			dot += logP[t][i] * logP[t-1][i]
			na += logP[t][i] * logP[t][i]
			nb2 += logP[t-1][i] * logP[t-1][i]
		}
		corr += dot / (math.Sqrt(na)*math.Sqrt(nb2) + 1e-12)
	}
	corr /= float64(nFrames - 1)

	return dspMetrics{Flat: median(flat), Syll: syll, Corr: corr, ok: true}
}

// stftPower is scipy.signal.stft(x, nperseg=512, noverlap=256) squared, plus
// 1e-12: periodic Hann window, nperseg/2 zeros either side, the end padded out
// to a whole hop, and scipy's scaling by the window sum. The scaling matters
// only through the 1e-12 floor, which near-silent bins hit, but with it the
// metrics match NightDX's to within float rounding.
func stftPower(x []float32) ([][]float64, int) {
	pad := dspNperseg / 2
	n := len(x) + 2*pad
	if r := (n - dspNperseg) % dspHop; r != 0 {
		n += dspHop - r
	}
	buf := make([]float64, n)
	for i, v := range x {
		buf[pad+i] = float64(v)
	}
	w := hannPeriodic(dspNperseg)
	var wsum float64
	for _, v := range w {
		wsum += v
	}
	frames := (n-dspNperseg)/dspHop + 1
	out := make([][]float64, frames)
	fr := make([]complex128, dspNperseg)
	for t := 0; t < frames; t++ {
		off := t * dspHop
		for i := 0; i < dspNperseg; i++ {
			fr[i] = complex(buf[off+i]*w[i], 0)
		}
		fftRadix2(fr)
		row := make([]float64, dspNperseg/2+1)
		for k := range row {
			re, im := real(fr[k])/wsum, imag(fr[k])/wsum
			row[k] = re*re + im*im + 1e-12
		}
		out[t] = row
	}
	return out, frames
}

// welch is scipy.signal.welch's estimate shape: periodic Hann, half overlap,
// each segment's mean removed, periodograms averaged. Unnormalised: callers
// take ratios. Returns frequencies and powers for bins 0..nperseg/2.
func welch(x []float64, fs float64, nperseg int) ([]float64, []float64) {
	if nperseg < 2 {
		return nil, nil
	}
	size := 1
	for size < nperseg {
		size <<= 1
	}
	w := hannPeriodic(nperseg)
	step := nperseg - nperseg/2
	nb := nperseg/2 + 1
	pw := make([]float64, nb)
	segs := 0
	fr := make([]complex128, size)
	for start := 0; start+nperseg <= len(x); start += step {
		var mean float64
		for _, v := range x[start : start+nperseg] {
			mean += v
		}
		mean /= float64(nperseg)
		for i := range fr {
			fr[i] = 0
		}
		for i := 0; i < nperseg; i++ {
			fr[i] = complex((x[start+i]-mean)*w[i], 0)
		}
		if size == nperseg {
			fftRadix2(fr)
			for k := 0; k < nb; k++ {
				re, im := real(fr[k]), imag(fr[k])
				pw[k] += re*re + im*im
			}
		} else {
			// nperseg not a power of two (a short window): direct DFT.
			for k := 0; k < nb; k++ {
				var re, im float64
				for i := 0; i < nperseg; i++ {
					a := -2 * math.Pi * float64(k*i) / float64(nperseg)
					v := real(fr[i])
					re += v * math.Cos(a)
					im += v * math.Sin(a)
				}
				pw[k] += re*re + im*im
			}
		}
		segs++
	}
	freqs := make([]float64, nb)
	for k := range freqs {
		freqs[k] = float64(k) * fs / float64(nperseg)
		if segs > 0 {
			pw[k] /= float64(segs)
		}
	}
	return freqs, pw
}

func hannPeriodic(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return w
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}
