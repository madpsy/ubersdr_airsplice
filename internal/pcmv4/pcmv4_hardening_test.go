package pcmv4

import "testing"

// Hardening tests from clients/hpsdr-go/internal/pcmv4: every length in a
// packet comes off a socket, so a bad one must be an error, never a panic or an
// unbounded allocation.

// A truncated packet must be refused rather than read past.
func TestPCMv4TruncatedPacketsAreRefused(t *testing.T) {
	first := readV4Fixture(t, "pcmv4_stream.bin")[0]
	for n := 0; n < len(first); n++ {
		if _, _, err := NewPCMv4StreamDecoder().DecodePacket(first[:n]); err == nil {
			t.Fatalf("a packet cut to %d of %d bytes was accepted", n, len(first))
		}
	}
}

// A scaled packet whose shift byte is missing must be refused rather than read
// as the first byte of the body.
func TestPCMv4ScaledRejectsMissingShift(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_scaled.bin")
	hdr, _, err := NewPCMv4StreamDecoder().DecodePacket(packets[0])
	if err != nil {
		t.Fatalf("packet 0: %v", err)
	}
	if hdr.Profile != PredProfileIQScaled {
		t.Skip("fixture no longer opens with a scaled packet")
	}
	_, off, err := NewPCMv4HeaderDecoder().Decode(packets[0])
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if _, _, err := NewPCMv4StreamDecoder().DecodePacket(packets[0][:off]); err == nil {
		t.Fatal("a scaled packet with no shift byte was accepted")
	}
}

// Random corruption must produce errors, never a panic. Before MaxSampleCount a
// corrupted sample-count uvarint could size an allocation large enough to kill
// the process.
func TestPCMv4GarbageDoesNotPanic(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_stream.bin")
	var seed uint32 = 1
	for i := 0; i < 2000; i++ {
		pkt := append([]byte(nil), packets[i%len(packets)]...)
		for j := 5; j < len(pkt); j++ {
			seed = seed*1664525 + 1013904223
			if seed>>28 == 0 {
				pkt[j] = byte(seed >> 8)
			}
		}
		dec := NewPCMv4StreamDecoder()
		_, _, _ = dec.DecodePacket(packets[0])
		_, _, _ = dec.DecodePacket(pkt)
	}
}
