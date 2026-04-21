package main

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/klauspost/compress/zstd"
)

// ---------------------------------------------------------------------------
// PCM binary packet decoder
// ---------------------------------------------------------------------------
// The UberSDR server sends packets in the ubersdr hybrid binary format.
// Two packet types:
//
//	Full header v1 (magic 0x5043 "PC", 29 bytes):
//	  [0:2]   uint16  magic
//	  [2]     uint8   version
//	  [3]     uint8   format (0=PCM, 2=PCM-zstd)
//	  [4:12]  uint64  RTP timestamp (LE)
//	  [12:20] uint64  wall-clock ms (LE)
//	  [20:24] uint32  sample rate (LE)
//	  [24]    uint8   channels
//	  [25:29] uint32  reserved
//	  [29:]   []byte  PCM samples (big-endian int16)
//
//	Full header v2 (37 bytes) adds signal quality fields:
//	  [25:29] float32 baseband power dBFS   ← extracted for SNR accumulator
//	  [29:33] float32 noise density dBFS    ← extracted for SNR accumulator
//	  [33:37] uint32  reserved
//	  [37:]   []byte  PCM samples (big-endian int16)
//
//	Minimal header (magic 0x504D "PM", 13 bytes):
//	  [0:2]   uint16  magic
//	  [2]     uint8   version
//	  [3:11]  uint64  RTP timestamp (LE)
//	  [11:13] uint16  reserved
//	  [13:]   []byte  PCM samples (big-endian int16)

const (
	magicFull    = 0x5043 // "PC"
	magicMinimal = 0x504D // "PM"
)

// pcmPacket is the result of decoding one binary WebSocket message.
type pcmPacket struct {
	pcm          []byte  // little-endian int16 PCM samples
	sampleRate   int
	channels     int
	hasSigInfo   bool    // true only for v2 full-header packets
	basebandDBFS float32 // baseband power dBFS (v2 only)
	noiseDBFS    float32 // noise density dBFS (v2 only)
}

type pcmDecoder struct {
	zd           *zstd.Decoder
	lastRate     int
	lastChannels int
}

func newPCMDecoder() (*pcmDecoder, error) {
	zd, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("zstd init: %w", err)
	}
	return &pcmDecoder{zd: zd}, nil
}

// decode decompresses (if needed) and parses a binary PCM packet.
// Returns a pcmPacket with little-endian int16 PCM bytes and signal info.
func (d *pcmDecoder) decode(data []byte, isZstd bool) (pcmPacket, error) {
	if isZstd {
		var err error
		data, err = d.zd.DecodeAll(data, nil)
		if err != nil {
			return pcmPacket{}, fmt.Errorf("zstd decompress: %w", err)
		}
	}

	if len(data) < 4 {
		return pcmPacket{}, fmt.Errorf("packet too short (%d bytes)", len(data))
	}

	magic := binary.LittleEndian.Uint16(data[0:2])

	var pkt pcmPacket
	var raw []byte

	switch magic {
	case magicFull:
		version := data[2]
		var headerLen int
		switch version {
		case 2:
			headerLen = 37
		default: // version 1
			headerLen = 29
		}
		if len(data) < headerLen {
			return pcmPacket{}, fmt.Errorf("full-header packet too short (%d < %d)", len(data), headerLen)
		}
		pkt.sampleRate = int(binary.LittleEndian.Uint32(data[20:24]))
		pkt.channels = int(data[24])
		raw = data[headerLen:]
		d.lastRate = pkt.sampleRate
		d.lastChannels = pkt.channels

		if version == 2 {
			pkt.hasSigInfo = true
			pkt.basebandDBFS = math.Float32frombits(binary.LittleEndian.Uint32(data[25:29]))
			pkt.noiseDBFS = math.Float32frombits(binary.LittleEndian.Uint32(data[29:33]))
		}

	case magicMinimal:
		if len(data) < 13 {
			return pcmPacket{}, fmt.Errorf("minimal-header packet too short (%d bytes)", len(data))
		}
		raw = data[13:]
		pkt.sampleRate = d.lastRate
		pkt.channels = d.lastChannels
		if pkt.sampleRate == 0 || pkt.channels == 0 {
			return pcmPacket{}, fmt.Errorf("minimal header received before full header")
		}

	default:
		return pcmPacket{}, fmt.Errorf("unknown magic 0x%04X", magic)
	}

	// Convert big-endian int16 → little-endian int16
	n := len(raw) / 2
	le := make([]byte, len(raw))
	for i := 0; i < n; i++ {
		s := binary.BigEndian.Uint16(raw[i*2:])
		binary.LittleEndian.PutUint16(le[i*2:], s)
	}
	pkt.pcm = le
	return pkt, nil
}

func (d *pcmDecoder) close() { d.zd.Close() }

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
