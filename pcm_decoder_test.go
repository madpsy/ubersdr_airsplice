package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Integration tests for the version 4 migration: the URL that asks for it, and
// the pcmDecoder wrapper that consumes what comes back.
//
// The codec itself is verified in internal/pcmv4; what is checked here is the
// wiring, which is where a migration actually goes wrong -- a version left at 2,
// a decoder shared across reconnects, a sample rate no longer reaching the WAV
// header.

// pcmv4StreamSHA is the SHA-256 of the fixture's samples as little-endian
// int16, the same constant the codec package and every other port of this
// decoder check against.
const pcmv4StreamSHA = "4875d2185f1ff5a2031386c569cac0c2259e6a827b9e61f813399a19c3b9c903"

// readFixturePackets returns the packets in testdata/pcmv4_stream.bin.
// Layout: "UV4F", a format byte, a uint32 packet count, then each packet as a
// uint32 length and that many bytes.
func readFixturePackets(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "pcmv4_stream.bin"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(raw) < 9 || string(raw[:4]) != "UV4F" || raw[4] != 0 {
		t.Fatal("fixture: bad header")
	}
	count := int(binary.LittleEndian.Uint32(raw[5:]))
	off := 9
	packets := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		n := int(binary.LittleEndian.Uint32(raw[off:]))
		off += 4
		packets = append(packets, raw[off:off+n])
		off += n
	}
	return packets
}

// The stream URL must ask for version 4 and keep the lossless format name.
// "pcm-zstd" still selects the lossless path on the server; only the version
// moves, and a stale "2" here would be served happily as the old wire format
// and then fail to parse on every single packet.
func TestWSURLRequestsProtocolV4(t *testing.T) {
	inst := newInstance(14230000, 0, "usb", "https://example.org", "", "", 0)
	raw := inst.wsURL()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("wsURL produced an unparseable URL %q: %v", raw, err)
	}
	if u.Scheme != "wss" {
		t.Errorf("scheme: got %q, want wss", u.Scheme)
	}
	q := u.Query()
	if got := q.Get("version"); got != "4" {
		t.Errorf("version: got %q, want 4", got)
	}
	if got := q.Get("format"); got != "pcm-zstd" {
		t.Errorf("format: got %q, want pcm-zstd", got)
	}
	if got := q.Get("mode"); got != "usb" {
		t.Errorf("mode: got %q, want usb", got)
	}
	if got := q.Get("frequency"); got != "14230000" {
		t.Errorf("frequency: got %q, want 14230000", got)
	}
	if strings.Contains(raw, "version=2") {
		t.Errorf("URL still asks for the old protocol version: %s", raw)
	}
}

// The decoder must render the server's stream sample for sample, through the
// same wrapper the receive loop uses -- and hand on the rate and channel count
// the header carries, which is all the recorder has now that they are no longer
// in a fixed header field, and what every WAV header it writes is built from.
func TestPCMDecoderMatchesServerStream(t *testing.T) {
	packets := readFixturePackets(t)
	dec, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("newPCMDecoder: %v", err)
	}
	defer dec.close()

	h := sha256.New()
	sawStereo := false
	rates := map[int]bool{}
	for i, pkt := range packets {
		p, err := dec.decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if p.sampleRate <= 0 || p.channels <= 0 {
			t.Fatalf("packet %d: stream parameters lost (rate %d, channels %d)", i, p.sampleRate, p.channels)
		}
		rates[p.sampleRate] = true
		if len(p.pcm)%(2*p.channels) != 0 {
			t.Fatalf("packet %d: %d bytes is not whole frames of %d channels", i, len(p.pcm), p.channels)
		}
		if p.channels == 2 {
			sawStereo = true
			// wfm arrives stereo and is downmixed before it reaches the WAV
			// writer; a packet that is not whole frames would slip a channel.
			if got, want := len(downmixStereoToMono(p.pcm)), len(p.pcm)/2; got != want {
				t.Fatalf("packet %d: downmix produced %d bytes, want %d", i, got, want)
			}
		}
		h.Write(p.pcm)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4StreamSHA {
		t.Fatalf("decoded samples differ from what the server encoded\n got %s\nwant %s", got, pcmv4StreamSHA)
	}
	if !sawStereo {
		t.Error("fixture carried no stereo, so the wfm downmix path went untested")
	}
	if len(rates) < 2 {
		t.Errorf("fixture reported only %d sample rate(s); the carried-forward metadata is untested", len(rates))
	}
}

// profileStablePrefix is a run of fixture packets that never changes codec
// profile.
//
// It has to be measured rather than guessed. The fixture switches profile at
// packet 155, and PCMv4StreamDecoder builds a fresh PredictiveCodec whenever
// the profile changes -- which incidentally throws away exactly the adaptation
// this test is trying to prove persists. Compare across that boundary and a
// carried-over decoder converges back onto the expected samples on its own, so
// the test passes whether or not the reset happens. Staying inside one profile
// is what makes the divergence real.
const profileStablePrefix = 50

// Reconnecting must start the codec from nothing. The predictor is backward
// adaptive, so a decoder carried across a reconnect decodes the new stream
// against the old one's taps: no error, just a WAV file full of noise. runOnce
// builds a fresh decoder per dial; reset() is the same thing for a caller that
// keeps one, and this is the property both rely on.
func TestPCMDecoderResetClearsStreamState(t *testing.T) {
	packets := readFixturePackets(t)
	if len(packets) <= profileStablePrefix {
		t.Fatalf("fixture has only %d packets", len(packets))
	}

	hashN := func(dec *pcmDecoder, n int) string {
		h := sha256.New()
		for i, pkt := range packets[:n] {
			p, err := dec.decode(pkt)
			if err != nil {
				t.Fatalf("packet %d: %v", i, err)
			}
			h.Write(p.pcm)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	hashAll := func(dec *pcmDecoder) string { return hashN(dec, len(packets)) }

	// What the prefix decodes to from a standing start.
	clean, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("newPCMDecoder: %v", err)
	}
	defer clean.close()
	wantPrefix := hashN(clean, profileStablePrefix)

	dec, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("newPCMDecoder: %v", err)
	}
	defer dec.close()

	// A connection that died partway through, all of it inside one profile.
	for i, pkt := range packets[:profileStablePrefix] {
		if _, err := dec.decode(pkt); err != nil {
			t.Fatalf("priming packet %d: %v", i, err)
		}
	}

	// Replaying without a reset must NOT reproduce the prefix; if it did, the
	// reset would be untestable and this test would be worthless.
	if hashN(dec, profileStablePrefix) == wantPrefix {
		t.Fatal("a half-consumed decoder reproduced the stream, so the reset cannot be shown to matter")
	}

	// After reset — equivalently, on the fresh decoder runOnce builds — it does.
	dec.reset()
	if got := hashAll(dec); got != pcmv4StreamSHA {
		t.Fatalf("reset did not clear the stream state\n got %s\nwant %s", got, pcmv4StreamSHA)
	}

	fresh, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("newPCMDecoder: %v", err)
	}
	defer fresh.close()
	if got := hashAll(fresh); got != pcmv4StreamSHA {
		t.Fatalf("a per-connection decoder did not reproduce the stream\n got %s\nwant %s", got, pcmv4StreamSHA)
	}
}

// hasSigInfo gates the SNR accumulator. The version 4 header reports -999 when
// radiod measured nothing, and feeding that in would drag every average in the
// UI and the recorder's trigger down by ninety-odd dB.
func TestSignalQualitySentinelIsNotAccumulated(t *testing.T) {
	packets := readFixturePackets(t)
	dec, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("newPCMDecoder: %v", err)
	}
	defer dec.close()

	acc := &snrAccumulator{}
	for i, pkt := range packets {
		p, err := dec.decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if p.hasSigInfo && p.basebandDBFS <= -998 && p.noiseDBFS <= -998 {
			t.Fatalf("packet %d: hasSigInfo set with no reading at all", i)
		}
		if p.hasSigInfo {
			acc.add(p.basebandDBFS, p.noiseDBFS)
		}
	}

	stats := acc.peek()
	if stats.Count > 0 && stats.MinDB < -900 {
		t.Errorf("the -999 sentinel reached the accumulator: min SNR %.1f dB over %d samples",
			stats.MinDB, stats.Count)
	}
}

// A receiver older than 0.1.63 clamps the requested version to 1-3 and serves
// the zstd-wrapped version 1 shape without saying so. Naming that is the
// difference between an operator upgrading the server and one staring at a
// directory of silent recordings.
func TestPCMDecoderNamesPreV4Servers(t *testing.T) {
	dec, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("newPCMDecoder: %v", err)
	}
	defer dec.close()

	_, err = dec.decode([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00})
	if err == nil {
		t.Fatal("a zstd frame decoded as version 4")
	}
	if !strings.Contains(err.Error(), "zstd") {
		t.Errorf("error does not name the cause: %v", err)
	}
}
