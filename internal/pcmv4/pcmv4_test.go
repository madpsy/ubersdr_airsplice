package pcmv4

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Conformance test for the version 4 receive path.
//
// ../../testdata/pcmv4_stream.bin is a packet stream the SERVER's encoder
// produced, and pcmv4ExpectedSHA is the SHA-256 of the samples that went into
// it, little endian, exactly as DecodePacketLE renders them.
//
// It earns its 90 kB. The version 4 predictor is backward adaptive: the two
// ends derive their filter taps independently from the samples already coded
// and never exchange a coefficient, so any arithmetic difference between this
// decoder and the Go one on the server produces plausible noise rather than an
// error. Nothing short of comparing the samples would catch it: the recorder
// would keep writing perfectly well-formed WAV files full of hash, with nothing
// anywhere saying why.
//
// The stream covers what the format can do: the ordinary mono audio this addon
// records, silent packets carrying no body, an escape to verbatim samples on
// incompressible noise, a sample-rate change, and two interleaved channels --
// the shape wfm arrives in -- including the varying packet length that makes
// the header's sample count necessary, across the five-second periodic
// resynchronisation.
//
// The same fixture and the same expected hash are used by the Go, C++, Python
// and JavaScript ports of this decoder; a change here that is not made there is
// a divergence nothing else would report.
const pcmv4ExpectedSHA = "4875d2185f1ff5a2031386c569cac0c2259e6a827b9e61f813399a19c3b9c903"

// pcmv4RiceEdgeSHA is the same for testdata/pcmv4_rice_edge.bin, which covers
// what a recording of ordinary traffic will not: a Rice codeword whose unary
// run is exactly 63 bits long, counted out of a full 64-bit accumulator, so the
// decoder shifts by 64. It appeared about once every quarter of a million
// packets on live IQ, which is often enough to matter to a recorder that runs
// for months and rare enough that only a fixture will find it.
const pcmv4RiceEdgeSHA = "3413109ff6d06d44fb8fa44c84595b776f5570f05663b762830853ddc0183527"

// pcmv4ScaledSHA is the same for testdata/pcmv4_scaled.bin, the reduced-depth
// IQ mode a client opts into with min_margin: profile 2, where a shift byte
// leads the body and the samples come back shifted left by it. Getting the
// shift wrong does not fail; it delivers a signal several bits too quiet, which
// is exactly the kind of thing only a hash notices.
const pcmv4ScaledSHA = "7315366ceed3e70552c28d31cde690a14dc66f5244b5a8dc34a5e696f5698ccc"

// fixtureDir is the repository's testdata, shared with the package-level
// integration tests rather than duplicated per package.
func fixtureDir() string { return filepath.Join("..", "..", "testdata") }

// readV4Fixture returns the packets in one of the fixture streams.
//
// Layout: "UV4F", a format byte, a uint32 packet count, then each packet as a
// uint32 length and that many bytes.
func readV4Fixture(t *testing.T, name string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir(), name))
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
		if off+4 > len(raw) {
			t.Fatalf("fixture: truncated length at packet %d", i)
		}
		n := int(binary.LittleEndian.Uint32(raw[off:]))
		off += 4
		if off+n > len(raw) {
			t.Fatalf("fixture: truncated packet %d", i)
		}
		packets = append(packets, raw[off:off+n])
		off += n
	}
	if off != len(raw) {
		t.Fatalf("fixture: %d trailing bytes", len(raw)-off)
	}
	return packets
}

// hashStream decodes every packet through one decoder and returns the SHA-256
// of the little-endian samples, plus each distinct (rate, channels) in order.
func hashStream(t *testing.T, dec *PCMv4StreamDecoder, packets [][]byte) (string, [][2]int) {
	t.Helper()
	h := sha256.New()
	var params [][2]int
	for i, pkt := range packets {
		if !PCMv4IsHeader(pkt) {
			t.Fatalf("packet %d not recognised as version 4", i)
		}
		pcmLE, rate, channels, _, _, err := dec.DecodePacketLE(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if len(pcmLE) == 0 || len(pcmLE)%(2*channels) != 0 {
			t.Fatalf("packet %d: %d bytes is not whole frames of %d channels", i, len(pcmLE), channels)
		}
		p := [2]int{rate, channels}
		if len(params) == 0 || params[len(params)-1] != p {
			params = append(params, p)
		}
		h.Write(pcmLE)
	}
	return hex.EncodeToString(h.Sum(nil)), params
}

func TestPCMv4DecodesServerStream(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_stream.bin")

	// Every distinct (rate, channels) the fixture passes through, in order. A
	// decoder that lost the carried-forward metadata could still hash correctly
	// while mislabelling the stream, and the sample rate is what the detector's
	// timings are built on.
	wantParams := [][2]int{{12000, 1}, {24000, 1}, {384000, 2}}

	got, gotParams := hashStream(t, NewPCMv4StreamDecoder(), packets)
	if got != pcmv4ExpectedSHA {
		t.Fatalf("decoded samples differ from what the server encoded\n got %s\nwant %s", got, pcmv4ExpectedSHA)
	}
	if len(gotParams) != len(wantParams) {
		t.Fatalf("stream parameters: got %v, want %v", gotParams, wantParams)
	}
	for i := range wantParams {
		if gotParams[i] != wantParams[i] {
			t.Fatalf("stream parameters: got %v, want %v", gotParams, wantParams)
		}
	}
}

// The reduced-depth mode. This addon records demodulated audio and so never
// asks the server for it -- profile 2 arrives only for a client that sent
// min_margin -- but the decoder is the same one every other port ships, and a
// profile it declines to decode is a hard error rather than a fallback. The
// fixture covers the paths that exist only there: a shift byte leading the
// body, a shift that changes as the margin does, a silent packet that carries
// no shift at all, an escape that carries one, and the profile switching to
// plain IQ and back when the margin goes to lossless.
func TestPCMv4DecodesScaledStream(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_scaled.bin")
	got, _ := hashStream(t, NewPCMv4StreamDecoder(), packets)
	if got != pcmv4ScaledSHA {
		t.Fatalf("scaled stream decoded wrongly\n got %s\nwant %s", got, pcmv4ScaledSHA)
	}
}

// The Rice edge case: a 63-bit unary run shifted out of a full accumulator.
func TestPCMv4DecodesRiceEdgeStream(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_rice_edge.bin")
	got, _ := hashStream(t, NewPCMv4StreamDecoder(), packets)
	if got != pcmv4RiceEdgeSHA {
		t.Fatalf("Rice edge stream decoded wrongly\n got %s\nwant %s", got, pcmv4RiceEdgeSHA)
	}
}

// profileStablePrefix is a run of fixture packets that never changes codec
// profile.
//
// It has to be measured rather than guessed. The fixture switches profile at
// packet 155, and DecodePacket builds a fresh PredictiveCodec whenever the
// profile changes -- which incidentally throws away exactly the adaptation the
// test below is trying to prove persists. Compare across that boundary and a
// carried-over decoder converges back onto the expected samples on its own, so
// the test would pass whether or not the reset happened. Staying inside one
// profile is what makes the divergence real.
const profileStablePrefix = 50

// A fresh decoder must reproduce the stream exactly, whatever a previous
// decoder was left holding. This is the property the receive loop depends on
// when it resets on reconnect: a half-consumed stream leaves the predictor
// adapted and the header baseline moved, and carrying either into the next
// connection would decode it as plausible noise without ever erroring.
func TestPCMv4DecoderStateDoesNotSurviveReset(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_stream.bin")
	if len(packets) <= profileStablePrefix {
		t.Fatalf("fixture has only %d packets", len(packets))
	}
	prefix := packets[:profileStablePrefix]

	// What the prefix decodes to from a standing start.
	wantPrefix, _ := hashStream(t, NewPCMv4StreamDecoder(), prefix)

	// Consume part of a stream, exactly as a dropped connection would, staying
	// within one profile so no codec rebuild can wipe the state for us.
	stale := NewPCMv4StreamDecoder()
	for i, pkt := range prefix {
		if _, _, err := stale.DecodePacket(pkt); err != nil {
			t.Fatalf("priming packet %d: %v", i, err)
		}
	}

	// The stale decoder replayed from the top does NOT reproduce the prefix:
	// that is what makes the reset load-bearing rather than tidiness.
	staleHash, _ := hashStream(t, stale, prefix)
	if staleHash == wantPrefix {
		t.Fatal("a half-consumed decoder reproduced the stream, so this test proves nothing about the reset")
	}

	// A replacement decoder — what pcmDecoder.reset() installs — does, over the
	// whole stream.
	freshHash, _ := hashStream(t, NewPCMv4StreamDecoder(), packets)
	if freshHash != pcmv4ExpectedSHA {
		t.Fatalf("a fresh decoder did not reproduce the stream\n got %s\nwant %s", freshHash, pcmv4ExpectedSHA)
	}
}

// A packet arriving before any resynchronisation point is rejected rather than
// guessed at, which is what a decoder that was NOT reset would be doing.
func TestPCMv4RejectsDeltaBeforeResync(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_stream.bin")
	dec := NewPCMv4StreamDecoder()

	// Find the first packet that is not itself a resynchronisation point.
	const metadataBit = 1 << 5
	for _, pkt := range packets {
		if len(pkt) > 4 && pkt[4]&metadataBit == 0 {
			if _, _, err := dec.DecodePacket(pkt); err == nil {
				t.Fatal("a delta packet decoded against a decoder that had seen no metadata")
			}
			return
		}
	}
	t.Skip("fixture has no delta packets")
}

// A server too old for version 4 answers with the zstd-wrapped version 1 shape.
// Recognising it is what lets the addon say why rather than logging a bad magic
// for every packet.
func TestLegacyServerFramesAreRecognised(t *testing.T) {
	zstd := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00}
	if !IsZstdFrame(zstd) || PCMv4IsHeader(zstd) {
		t.Error("a zstd frame was misclassified")
	}
	for _, pkt := range readV4Fixture(t, "pcmv4_stream.bin") {
		if IsZstdFrame(pkt) {
			t.Fatal("a version 4 packet read as zstd")
		}
	}
	for _, short := range [][]byte{nil, {}, {0x50}, {0x50, 0x43, 0x4D}} {
		if PCMv4IsHeader(short) || IsZstdFrame(short) {
			t.Errorf("short frame %v misclassified", short)
		}
	}
}
