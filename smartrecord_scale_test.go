package main

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The Smart Record thresholds are calibrated against the SNR scale the server
// reports, and that scale moved twice: once when this addon went from audio
// protocol version 2 to version 4 (noise density -> noise power, a ~34 dB shift
// documented in main.go), and once when the server stopped clamping its noise
// figure at a floor, letting it read its true, much lower value.
//
// Arithmetic alone got this wrong. Rescaling the old 50/35 defaults by the
// units correction gives 16/1, and the live receiver says 16 is above a normal
// SSB QSO while 1 is buried inside the idle spread. So the constants below are
// MEASURED populations, and the thresholds are asserted against them rather
// than against a formula.
//
// The failure being pinned is silent in both directions: a start threshold too
// high means the gate never opens and the channel records nothing, and a stop
// threshold inside the noise means the tail countdown is always cancelled and a
// recording never closes. Neither logs an error.

// ---------------------------------------------------------------------------
// Measured on m9psy.tunnel.ubersdr.org, 2026-09-03, through this addon's own
// v4 decoder, sampling the gate-visible statistic (mean of the last two
// packets, as peekLatest(2) computes it) at the 2.4 kHz SSB passband these
// defaults are for.
// ---------------------------------------------------------------------------

const (
	// IDLE: six captures totalling ~20,000 packets on 7.13, 7.18, 14.2 and
	// 13.9 MHz plus two 90 s captures on 7.12 and 7.15 MHz. These are the
	// worst (highest) value each statistic took across all six.
	measuredIdleMedianMax = 2.0  // medians spanned -0.8 .. +2.0
	measuredIdleP90Max    = 3.8  // p90 spanned 0.3 .. 3.8
	measuredIdleP99Max    = 7.1  // p99 spanned 1.1 .. 7.1
	measuredIdleAbsMax    = 12.9 // single-sample outlier; cannot sustain

	// MODERATE SSB QSO: 7.150 MHz LSB with a station working.
	measuredQSOMedian = 7.95
	measuredQSOP95    = 12.94

	// STRONG SIGNAL: measured independently at 12 kHz USB.
	measuredStrongMedian = 32.67

	// The v2-era defaults, kept so the size of the move is visible.
	v2ThresholdStart = 50.0
	v2ThresholdStop  = 35.0
)

// TestStartThresholdSitsBetweenIdleAndSignal pins the start threshold into the
// gap the measurements actually found.
func TestStartThresholdSitsBetweenIdleAndSignal(t *testing.T) {
	start := float64(defaultSmartStartThreshDB)

	// Above the idle population, or the gate opens on noise. p99 is the right
	// bound: the absolute max is a single sample and cannot survive the
	// start_hold_sec sustain requirement.
	if start <= measuredIdleP99Max {
		t.Errorf("start %.1f is not above the measured idle p99 %.1f — the gate would open on noise",
			start, measuredIdleP99Max)
	}

	// Below a normal QSO's p95, or only exceptionally strong stations record.
	// This is the check that 16 failed.
	if start >= measuredQSOP95 {
		t.Errorf("start %.1f is at or above a normal SSB QSO's p95 %.1f — normal QSOs would never trigger",
			start, measuredQSOP95)
	}

	// And well clear of a strong signal, which must trigger trivially.
	if start >= measuredStrongMedian/2 {
		t.Errorf("start %.1f is high relative to a strong signal's median %.1f",
			start, measuredStrongMedian)
	}

	if start != 10.0 {
		t.Errorf("start threshold: got %.1f, want 10", start)
	}
}

// TestStopThresholdClearsTheIdleSpread pins the stop threshold above the noise
// it has to distinguish itself from.
func TestStopThresholdClearsTheIdleSpread(t *testing.T) {
	stop := float64(defaultSmartStopThreshDB)

	// The tail cancels if any tick rises back above stop, so stop must sit
	// above the bulk of the idle distribution. p90 is the operative bound:
	// below it, ordinary noise cancels the countdown and the recording never
	// closes. This is the check that 1 failed.
	if stop <= measuredIdleP90Max {
		t.Errorf("stop %.1f is inside the measured idle spread (p90 %.1f) — the tail would never complete",
			stop, measuredIdleP90Max)
	}
	if stop <= measuredIdleMedianMax {
		t.Errorf("stop %.1f is at or below the idle median %.1f", stop, measuredIdleMedianMax)
	}

	// Below the sustained level of a normal QSO, or speech would be chopped.
	if stop >= measuredQSOMedian {
		t.Errorf("stop %.1f is at or above a normal QSO's median %.1f — speech would be cut short",
			stop, measuredQSOMedian)
	}

	if stop != 5.0 {
		t.Errorf("stop threshold: got %.1f, want 5", stop)
	}
}

// TestHysteresisIsDeliberate. The gap is 5 dB, not the 15 dB inherited from the
// version 2 scale. The measurements leave only ~10 dB between the top of the
// idle spread and a sustained QSO, so a 15 dB gap cannot fit without pushing one
// threshold onto the wrong side of its population.
func TestHysteresisIsDeliberate(t *testing.T) {
	start := float64(defaultSmartStartThreshDB)
	stop := float64(defaultSmartStopThreshDB)
	gap := start - stop

	if gap != 5.0 {
		t.Errorf("hysteresis gap: got %.1f dB, want 5", gap)
	}
	if gap <= 0 {
		t.Fatalf("start %.1f is not above stop %.1f", start, stop)
	}

	// The usable range the measurements left us. If a future re-tune widens the
	// gap past it, one threshold has necessarily left its population.
	usable := measuredQSOP95 - measuredIdleP99Max
	if gap > usable {
		t.Errorf("gap %.1f dB exceeds the %.1f dB measured between idle p99 and QSO p95; "+
			"one threshold must be on the wrong side", gap, usable)
	}
}

// TestThresholdsAreNotOnTheVersion2Scale is the tripwire for the original bug:
// a threshold left on the old scale is ~34 dB above anything the server now
// reports.
func TestThresholdsAreNotOnTheVersion2Scale(t *testing.T) {
	if float64(defaultSmartStartThreshDB) > measuredIdleAbsMax+v2ThresholdStart/2 {
		t.Errorf("start %.1f looks like it is still on the version 2 scale (was %.0f)",
			defaultSmartStartThreshDB, v2ThresholdStart)
	}
	if float64(defaultSmartStopThreshDB) > measuredIdleAbsMax+v2ThresholdStop/2 {
		t.Errorf("stop %.1f looks like it is still on the version 2 scale (was %.0f)",
			defaultSmartStopThreshDB, v2ThresholdStop)
	}
	// Both must be reachable at all: below the strongest thing measured.
	for name, v := range map[string]float32{
		"start": defaultSmartStartThreshDB,
		"stop":  defaultSmartStopThreshDB,
	} {
		if float64(v) >= measuredStrongMedian {
			t.Errorf("%s threshold %.1f exceeds even a strong signal's median %.1f",
				name, v, measuredStrongMedian)
		}
	}
}

// TestPerModeCorrectionsAreConsistent checks the units table documented in
// main.go against bandwidthParams. The v2->v4 correction is separate from the
// measured calibration above and still governs anyone migrating an old value.
func TestPerModeCorrectionsAreConsistent(t *testing.T) {
	cases := []struct {
		mode     string
		bwHz     int
		wantCorr float64
	}{
		{"cw", 500, 27.0},
		{"usb", 2700, 33.8},
		{"lsb", 2700, 33.8},
		{"am", 5000, 37.0},
		{"sam", 5000, 37.0},
		{"fm", 8000, 39.0},
		{"nfm", 8000, 39.0},
	}
	for _, c := range cases {
		low, high := bandwidthParams(c.mode, c.bwHz)
		passband := float64(high - low)
		if passband <= 0 {
			t.Errorf("%s: bandwidthParams gave a non-positive passband %.0f Hz", c.mode, passband)
			continue
		}
		corr := 10 * math.Log10(passband)
		if math.Abs(corr-c.wantCorr) > 0.1 {
			t.Errorf("%s: passband %.0f Hz gives a %.2f dB correction, main.go documents %.1f",
				c.mode, passband, corr, c.wantCorr)
		}
	}
}

// The numbers a user actually meets are the ones baked into the web UI, not the
// Go constants -- nothing on the Go side applies a default. If these drift apart
// the documentation is right and the product is wrong.
func TestUIDefaultsMatchTheGoConstants(t *testing.T) {
	js, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("static/app.js: %v", err)
	}
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("static/index.html: %v", err)
	}

	find := func(t *testing.T, what, pattern string, src []byte) float64 {
		t.Helper()
		m := regexp.MustCompile(pattern).FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s: no match for %q -- the default moved or was reformatted", what, pattern)
		}
		v, err := strconv.ParseFloat(string(m[1]), 64)
		if err != nil {
			t.Fatalf("%s: %q is not a number: %v", what, m[1], err)
		}
		return v
	}

	if got := find(t, "app.js start", `start_thresh_db\s*!= null \? sr\.start_thresh_db\s*: ([-\d.]+)`, js); got != float64(defaultSmartStartThreshDB) {
		t.Errorf("static/app.js start default is %v, Go constant is %v", got, defaultSmartStartThreshDB)
	}
	if got := find(t, "app.js stop", `stop_thresh_db\s*!= null \? sr\.stop_thresh_db\s*: ([-\d.]+)`, js); got != float64(defaultSmartStopThreshDB) {
		t.Errorf("static/app.js stop default is %v, Go constant is %v", got, defaultSmartStopThreshDB)
	}
	if got := find(t, "index.html start", `id="add-sr-start-thresh"\s+value="([-\d.]+)"`, html); got != float64(defaultSmartStartThreshDB) {
		t.Errorf("static/index.html start default is %v, Go constant is %v", got, defaultSmartStartThreshDB)
	}
	if got := find(t, "index.html stop", `id="add-sr-stop-thresh"\s+value="([-\d.]+)"`, html); got != float64(defaultSmartStopThreshDB) {
		t.Errorf("static/index.html stop default is %v, Go constant is %v", got, defaultSmartStopThreshDB)
	}
}
