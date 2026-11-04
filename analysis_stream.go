// analysis_stream.go — reading segments as a stream, so analysis memory does
// not grow with segment length.
//
// A segment can be an hour long (SEGMENT_SECS caps it there): ~170 MB of WAV
// at 24 kHz, and several times that once converted and resampled if read
// whole. Instead the WAV is read a chunk at a time, resampled by a resampler
// that carries its filter state from chunk to chunk, and handed out in the
// pieces each consumer wants: 10 s windows for EfficientAT, 5 s for the DSP
// classifier, 100 ms frames for Whisper. Peak memory is a few windows.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// audioSource yields mono float32 samples; read returns io.EOF once drained.
type audioSource interface {
	read(max int) ([]float32, error)
}

// sliceSource serves samples already in memory.
type sliceSource struct {
	x   []float32
	pos int
}

func (s *sliceSource) read(max int) ([]float32, error) {
	if s.pos >= len(s.x) {
		return nil, io.EOF
	}
	end := min(s.pos+max, len(s.x))
	out := s.x[s.pos:end]
	s.pos = end
	return out, nil
}

// wavStream reads a 16-bit PCM WAV file a chunk at a time, mixed to mono.
type wavStream struct {
	f         *os.File
	r         *bufio.Reader
	rate      int
	channels  int
	remaining int64 // data bytes left; -1 = to end of file (unfinalised header)
	raw       []byte
}

// openWAVStream parses the header and leaves the reader at the samples. A data
// chunk whose size was never finalised (the process died mid-segment) is read
// to the end of the file.
func openWAVStream(path string) (*wavStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	w := &wavStream{f: f, r: bufio.NewReaderSize(f, 256<<10)}
	if err := w.parseHeader(); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

func (w *wavStream) parseHeader() error {
	var hdr [12]byte
	if _, err := io.ReadFull(w.r, hdr[:]); err != nil {
		return fmt.Errorf("wav: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return errors.New("wav: not a RIFF/WAVE file")
	}
	var bits int
	haveFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(w.r, ch[:]); err != nil {
			return errors.New("wav: no data chunk")
		}
		id := string(ch[0:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch id {
		case "fmt ":
			if size < 16 || size > 1<<16 {
				return errors.New("wav: bad fmt chunk")
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(w.r, b); err != nil {
				return errors.New("wav: bad fmt chunk")
			}
			if format := binary.LittleEndian.Uint16(b[0:2]); format != 1 {
				return fmt.Errorf("wav: format %d is not PCM", format)
			}
			w.channels = int(binary.LittleEndian.Uint16(b[2:4]))
			w.rate = int(binary.LittleEndian.Uint32(b[4:8]))
			bits = int(binary.LittleEndian.Uint16(b[14:16]))
			haveFmt = true
			if size%2 == 1 {
				_, _ = w.r.Discard(1)
			}
		case "data":
			if !haveFmt {
				return errors.New("wav: data before fmt")
			}
			if bits != 16 || w.channels < 1 || w.rate <= 0 {
				return fmt.Errorf("wav: unsupported %d-bit %d-channel %d Hz", bits, w.channels, w.rate)
			}
			w.remaining = size
			if size == 0 || size == 0xFFFFFFFF {
				w.remaining = -1
			}
			return nil
		default:
			if _, err := w.r.Discard(int(size + size%2)); err != nil {
				return fmt.Errorf("wav: %w", err)
			}
		}
	}
}

func (w *wavStream) read(max int) ([]float32, error) {
	frame := 2 * w.channels
	want := int64(max * frame)
	if w.remaining >= 0 && want > w.remaining {
		want = w.remaining - w.remaining%int64(frame)
	}
	if want <= 0 {
		return nil, io.EOF
	}
	if int64(cap(w.raw)) < want {
		w.raw = make([]byte, want)
	}
	b := w.raw[:want]
	n, err := io.ReadFull(w.r, b)
	n -= n % frame
	if n == 0 {
		if err == nil || err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return nil, err
	}
	if w.remaining >= 0 {
		w.remaining -= int64(n)
	}
	out := make([]float32, n/frame)
	for i := range out {
		var acc int32
		for c := 0; c < w.channels; c++ {
			acc += int32(int16(binary.LittleEndian.Uint16(b[(i*w.channels+c)*2:])))
		}
		out[i] = float32(acc) / float32(w.channels) / 32768
	}
	return out, nil
}

func (w *wavStream) Close() error { return w.f.Close() }

// streamResampler is resample() fed in pieces: the same windowed-sinc filter
// (analysis_audio.go), keeping the input history the next outputs need. Fed
// all at once and flushed, it produces exactly what resample() does.
type streamResampler struct {
	pass       bool
	up, down   int64
	half, taps int64
	table      [][]float32
	buf        []float32 // input from global index bufStart
	bufStart   int64
	inN, outN  int64
}

func newStreamResampler(from, to int) *streamResampler {
	if from == to {
		return &streamResampler{pass: true}
	}
	g := gcd(from, to)
	up, down := to/g, from/g
	r := math.Min(1, float64(to)/float64(from)) * 0.95
	const zeros = 16
	half := int(math.Ceil(zeros / r))
	taps := 2*half + 1
	table := make([][]float32, up)
	for p := 0; p < up; p++ {
		frac := float64(p) / float64(up)
		h := make([]float32, taps)
		var sum float64
		for k := 0; k < taps; k++ {
			t := float64(k-half) - frac
			v := r * sinc(r*t) * blackman(t, float64(half)+1)
			h[k] = float32(v)
			sum += v
		}
		for k := range h {
			h[k] = float32(float64(h[k]) / sum)
		}
		table[p] = h
	}
	return &streamResampler{up: int64(up), down: int64(down), half: int64(half), taps: int64(taps), table: table}
}

// push adds input and returns every output sample it completes.
func (s *streamResampler) push(x []float32) []float32 {
	if s.pass {
		return append([]float32(nil), x...)
	}
	s.buf = append(s.buf, x...)
	s.inN += int64(len(x))
	return s.emit(false)
}

// flush returns the outputs that need input past the end (taken as zero).
func (s *streamResampler) flush() []float32 {
	if s.pass {
		return nil
	}
	return s.emit(true)
}

func (s *streamResampler) emit(final bool) []float32 {
	var out []float32
	total := s.inN * s.up / s.down // outputs the whole input yields
	for s.outN < total {
		pos := s.outN * s.down
		base := pos / s.up
		if !final && base+s.half >= s.inN {
			break // needs input not yet seen
		}
		h := s.table[pos%s.up]
		start := base - s.half
		k0, k1 := int64(0), s.taps
		if start < 0 {
			k0 = -start
		}
		if start+s.taps > s.inN {
			k1 = s.inN - start
		}
		var v float32
		if k1 > k0 {
			i0 := start + k0 - s.bufStart
			v = f32Dot(h[k0:k1], s.buf[i0:i0+(k1-k0)])
		}
		out = append(out, v)
		s.outN++
	}
	// Drop input no future output reaches.
	next := (s.outN*s.down)/s.up - s.half
	if drop := next - s.bufStart; drop > 0 {
		drop = min(drop, int64(len(s.buf)))
		s.buf = append(s.buf[:0], s.buf[drop:]...)
		s.bufStart += drop
	}
	return out
}

// resampledReader hands out a source's audio, resampled, in pieces of the
// size asked for.
type resampledReader struct {
	src   audioSource
	rs    *streamResampler
	buf   []float32
	eof   bool
	chunk int // source samples per read
}

func newResampledReader(src audioSource, from, to int) *resampledReader {
	return &resampledReader{src: src, rs: newStreamResampler(from, to), chunk: max(from, 1024)}
}

// next returns n samples, or fewer at the end; an empty slice once drained.
func (r *resampledReader) next(n int) ([]float32, error) {
	for len(r.buf) < n && !r.eof {
		x, err := r.src.read(r.chunk)
		if len(x) > 0 {
			r.buf = append(r.buf, r.rs.push(x)...)
		}
		if err == io.EOF {
			r.buf = append(r.buf, r.rs.flush()...)
			r.eof = true
		} else if err != nil {
			return nil, err
		}
	}
	k := min(n, len(r.buf))
	out := append([]float32(nil), r.buf[:k]...)
	r.buf = append(r.buf[:0], r.buf[k:]...)
	return out, nil
}

// forEachWindow calls visit on consecutive windows of win samples. The short
// window at the end is kept when it is over half a window long, or when it is
// the only one, and at least minLen samples (NightDX's windowing).
func forEachWindow(r *resampledReader, win, minLen int, visit func([]float32)) error {
	first := true
	for {
		x, err := r.next(win)
		if err != nil {
			return err
		}
		if len(x) == win {
			visit(x)
			first = false
			continue
		}
		if len(x) > 0 && (len(x) > win/2 || first) && len(x) >= minLen {
			visit(x)
		}
		return nil
	}
}
