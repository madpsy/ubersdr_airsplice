package main

import (
	"encoding/binary"
	"fmt"

	"github.com/madpsy/ubersdr_airsplice/internal/pcmv4"
)

// ---------------------------------------------------------------------------
// PCM binary packet decoder (audio protocol version 4)
// ---------------------------------------------------------------------------
// The UberSDR server sends one binary WebSocket message per packet. This addon
// asks for protocol version 4, which replaced the versions 1-3 shape entirely:
//
//	versions 1-3   a zstd frame wrapping a fixed 29- or 37-byte header and
//	               big-endian int16 samples
//	version 4      a "PCM4" magic, a variable-length header carrying only what
//	               changed since the last packet, and a body coded by the
//	               predictive lossless codec in internal/pcmv4
//
// zstd made this data larger rather than smaller -- it is an LZ77 matcher over
// bytes, and a band-limited RF signal has no repeated byte strings -- so the
// wrapper is gone and with it the byte swap: the codec emits little-endian
// int16 directly, which is what the WAV writer, the audio preview hub and the
// FFT already read.
//
// The sample rate and channel count now come from the header rather than a
// fixed field, and are carried forward between the server's periodic
// resynchronisation points, so every packet still reports them.
//
// STREAM LIFETIME
// ---------------
// The version 4 predictor is BACKWARD adaptive: both ends derive the filter
// taps from the samples already coded and never exchange a coefficient. A
// decoder instance therefore IS the stream. It must see every packet of its
// connection in order -- including ones whose samples are then discarded -- and
// it must not outlive the socket, or the taps carry one connection's adaptation
// into the next and the recording is plausible noise rather than an error.

// pcmPacket is the result of decoding one binary WebSocket message.
type pcmPacket struct {
	pcm          []byte  // little-endian int16 PCM samples
	sampleRate   int     // from the version 4 header, carried forward between resyncs
	channels     int     // 1 for demodulated audio, 2 for wfm stereo
	hasSigInfo   bool    // true when radiod actually reported signal quality
	basebandDBFS float32 // baseband power dBFS (-999 = no data)
	noiseDBFS    float32 // noise density dBFS (-999 = no data)
}

// pcmDecoder decodes one WebSocket connection's worth of version 4 packets.
//
// Not safe for concurrent use: it holds adaptation state, so it belongs to a
// single connection and a single goroutine. runOnce builds one per dial and
// drops it when the socket closes, which is the reset the format requires.
type pcmDecoder struct {
	stream *pcmv4.PCMv4StreamDecoder
}

func newPCMDecoder() (*pcmDecoder, error) {
	return &pcmDecoder{stream: pcmv4.NewPCMv4StreamDecoder()}, nil
}

// reset discards all stream state, so the decoder starts from nothing.
//
// A version 4 decoder carries predictor taps, the running timestamp and the
// last-seen metadata; reusing them across connections decodes the new stream
// against the old stream's adaptation, which sounds like noise and reports no
// error at all. runOnce gets its reset by constructing a decoder per
// connection; this exists for any caller that keeps one across dials.
func (d *pcmDecoder) reset() {
	d.stream = pcmv4.NewPCMv4StreamDecoder()
}

// decode parses one binary packet. Every binary message from the connection
// must be passed here, in order, even if the caller then drops the samples.
func (d *pcmDecoder) decode(data []byte) (pcmPacket, error) {
	if pcmv4.IsZstdFrame(data) {
		return pcmPacket{}, fmt.Errorf(
			"server sent a zstd frame: it predates audio protocol version %d (needs UberSDR 0.1.63 or later)",
			pcmv4.ProtocolVersion)
	}

	h, samples, err := d.stream.DecodePacket(data)
	if err != nil {
		return pcmPacket{}, err
	}

	pcm := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}

	return pcmPacket{
		pcm:        pcm,
		sampleRate: h.SampleRate,
		channels:   h.Channels,
		// -999 is the "radiod reported nothing" sentinel; anything above it is
		// a real reading, and only those belong in the SNR accumulator.
		hasSigInfo:   h.BasebandPower > -998 || h.Noise > -998,
		basebandDBFS: h.BasebandPower,
		noiseDBFS:    h.Noise,
	}, nil
}

// close releases the decoder. Version 4 owns nothing that needs freeing, but
// the call site is kept so the receive loop's shape does not change.
func (d *pcmDecoder) close() {}

// downmixStereoToMono converts 2-channel S16LE PCM to mono S16LE.
// Used for wfm mode which delivers stereo 48 kHz audio.
func downmixStereoToMono(stereo []byte) []byte {
	n := len(stereo) / 4 // 2 bytes per sample × 2 channels
	mono := make([]byte, n*2)
	for i := 0; i < n; i++ {
		l := int32(int16(binary.LittleEndian.Uint16(stereo[i*4:])))
		r := int32(int16(binary.LittleEndian.Uint16(stereo[i*4+2:])))
		m := int16((l + r) / 2)
		binary.LittleEndian.PutUint16(mono[i*2:], uint16(m))
	}
	return mono
}
