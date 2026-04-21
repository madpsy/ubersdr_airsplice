// ubersdr.go — Connect to an UberSDR WebSocket stream and receive demodulated
// PCM audio.  Includes a lightweight SNR accumulator fed from v2 full-header
// packets.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const rcvBufSize = 16 * 1024 * 1024 // 16 MiB SO_RCVBUF

// wsDialer sets SO_RCVBUF = 16 MiB on the underlying TCP socket.
var wsDialer = &websocket.Dialer{
	HandshakeTimeout: 10 * time.Second,
	NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		nd := &net.Dialer{}
		conn, err := nd.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			raw, err := tc.SyscallConn()
			if err == nil {
				_ = raw.Control(func(fd uintptr) {
					_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBufSize)
				})
			}
		}
		return conn, nil
	},
}

// ---------------------------------------------------------------------------
// Protocol types
// ---------------------------------------------------------------------------

type connectionCheckRequest struct {
	UserSessionID string `json:"user_session_id"`
	Password      string `json:"password,omitempty"`
}

type connectionCheckResponse struct {
	Allowed        bool     `json:"allowed"`
	Reason         string   `json:"reason,omitempty"`
	ClientIP       string   `json:"client_ip,omitempty"`
	Bypassed       bool     `json:"bypassed"`
	AllowedIQModes []string `json:"allowed_iq_modes,omitempty"`
	MaxSessionTime int      `json:"max_session_time"`
}

type wsMessage struct {
	Type      string `json:"type"`
	Error     string `json:"error,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Frequency int    `json:"frequency,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// ---------------------------------------------------------------------------
// snrAccumulator — collects per-packet SNR from v2 full-header packets
// ---------------------------------------------------------------------------

// SNRStats is a snapshot of accumulated SNR measurements.
type SNRStats struct {
	Count       int     `json:"count"`
	AvgDB       float32 `json:"avg_db"`
	MinDB       float32 `json:"min_db"`
	MaxDB       float32 `json:"max_db"`
	BasebandAvg float32 `json:"baseband_avg_dbfs"`
	NoiseAvg    float32 `json:"noise_avg_dbfs"`
}

// sanitiseFloat replaces NaN/Inf with 0 so the value is safe for JSON encoding.
func sanitiseFloat(f float32) float32 {
	if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
		return 0
	}
	return f
}

// Sanitise replaces any NaN/Inf fields with 0 in-place and returns the receiver.
func (s *SNRStats) Sanitise() *SNRStats {
	s.AvgDB = sanitiseFloat(s.AvgDB)
	s.MinDB = sanitiseFloat(s.MinDB)
	s.MaxDB = sanitiseFloat(s.MaxDB)
	s.BasebandAvg = sanitiseFloat(s.BasebandAvg)
	s.NoiseAvg = sanitiseFloat(s.NoiseAvg)
	return s
}

type snrSample struct {
	snrDB float32
	bb    float32
	noise float32
}

type snrAccumulator struct {
	mu      sync.Mutex
	samples []snrSample
}

func (a *snrAccumulator) add(baseband, noise float32) {
	if math.IsNaN(float64(baseband)) || math.IsNaN(float64(noise)) ||
		math.IsInf(float64(baseband), 0) || math.IsInf(float64(noise), 0) {
		return
	}
	snr := baseband - noise
	if math.IsNaN(float64(snr)) || math.IsInf(float64(snr), 0) {
		return
	}
	a.mu.Lock()
	a.samples = append(a.samples, snrSample{
		snrDB: snr,
		bb:    baseband,
		noise: noise,
	})
	a.mu.Unlock()
}

// drain returns the current stats and resets the accumulator.
func (a *snrAccumulator) drain() SNRStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.samples)
	if n == 0 {
		return SNRStats{}
	}
	var sumSNR, sumBB, sumN float32
	minSNR := float32(math.MaxFloat32)
	maxSNR := float32(-math.MaxFloat32)
	for _, s := range a.samples {
		sumSNR += s.snrDB
		sumBB += s.bb
		sumN += s.noise
		if s.snrDB < minSNR {
			minSNR = s.snrDB
		}
		if s.snrDB > maxSNR {
			maxSNR = s.snrDB
		}
	}
	fn := float32(n)
	a.samples = a.samples[:0]
	s := SNRStats{
		Count:       n,
		AvgDB:       sumSNR / fn,
		MinDB:       minSNR,
		MaxDB:       maxSNR,
		BasebandAvg: sumBB / fn,
		NoiseAvg:    sumN / fn,
	}
	s.Sanitise()
	return s
}

// peek returns stats without resetting (averaged over all accumulated samples).
func (a *snrAccumulator) peek() SNRStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.samples)
	if n == 0 {
		return SNRStats{}
	}
	var sumSNR, sumBB, sumN float32
	minSNR := float32(math.MaxFloat32)
	maxSNR := float32(-math.MaxFloat32)
	for _, s := range a.samples {
		sumSNR += s.snrDB
		sumBB += s.bb
		sumN += s.noise
		if s.snrDB < minSNR {
			minSNR = s.snrDB
		}
		if s.snrDB > maxSNR {
			maxSNR = s.snrDB
		}
	}
	fn := float32(n)
	s := SNRStats{
		Count:       n,
		AvgDB:       sumSNR / fn,
		MinDB:       minSNR,
		MaxDB:       maxSNR,
		BasebandAvg: sumBB / fn,
		NoiseAvg:    sumN / fn,
	}
	s.Sanitise()
	return s
}

// peekLatest returns stats for only the most recent N samples (up to last ~2s
// at 100ms packet rate = ~20 samples), giving a near-instantaneous reading
// suitable for real-time display. Does not reset the accumulator.
func (a *snrAccumulator) peekLatest(window int) SNRStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.samples)
	if n == 0 {
		return SNRStats{}
	}
	start := n - window
	if start < 0 {
		start = 0
	}
	slice := a.samples[start:]
	var sumSNR, sumBB, sumN float32
	minSNR := float32(math.MaxFloat32)
	maxSNR := float32(-math.MaxFloat32)
	for _, s := range slice {
		sumSNR += s.snrDB
		sumBB += s.bb
		sumN += s.noise
		if s.snrDB < minSNR {
			minSNR = s.snrDB
		}
		if s.snrDB > maxSNR {
			maxSNR = s.snrDB
		}
	}
	fn := float32(len(slice))
	st := SNRStats{
		Count:       len(slice),
		AvgDB:       sumSNR / fn,
		MinDB:       minSNR,
		MaxDB:       maxSNR,
		BasebandAvg: sumBB / fn,
		NoiseAvg:    sumN / fn,
	}
	st.Sanitise()
	return st
}

// ---------------------------------------------------------------------------
// fftBroadcastHub — fan-out of FFT magnitude frames to SSE listeners
// ---------------------------------------------------------------------------

type fftBroadcastHub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newFFTBroadcastHub() *fftBroadcastHub {
	return &fftBroadcastHub{clients: make(map[chan []byte]struct{})}
}

func (h *fftBroadcastHub) subscribe() chan []byte {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *fftBroadcastHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *fftBroadcastHub) broadcast(data []byte) {
	if len(data) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- data:
		default:
			// Subscriber too slow — drop frame.
		}
	}
}

func (h *fftBroadcastHub) hasListeners() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

// ---------------------------------------------------------------------------
// audioBroadcastHub — fan-out of raw PCM chunks to preview listeners
// ---------------------------------------------------------------------------

const audioPrerollChunks = 1

type audioBroadcastHub struct {
	mu        sync.Mutex
	clients   map[chan []byte]struct{}
	ring      [][]byte
	ringPos   int
	resetChan chan struct{}
}

func newAudioBroadcastHub() *audioBroadcastHub {
	return &audioBroadcastHub{
		clients:   make(map[chan []byte]struct{}),
		ring:      make([][]byte, audioPrerollChunks),
		resetChan: make(chan struct{}),
	}
}

func (h *audioBroadcastHub) subscribe() chan []byte {
	ch := make(chan []byte, audioPrerollChunks+64)
	h.mu.Lock()
	for i := 0; i < audioPrerollChunks; i++ {
		slot := (h.ringPos + i) % audioPrerollChunks
		if h.ring[slot] != nil {
			ch <- h.ring[slot]
		}
	}
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *audioBroadcastHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *audioBroadcastHub) resetClients() {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.resetChan)
	h.resetChan = make(chan struct{})
	for ch := range h.clients {
		close(ch)
	}
	h.clients = make(map[chan []byte]struct{})
	h.ring = make([][]byte, audioPrerollChunks)
	h.ringPos = 0
}

func (h *audioBroadcastHub) currentResetChan() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resetChan
}

func (h *audioBroadcastHub) broadcast(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	buf := make([]byte, len(pcm))
	copy(buf, pcm)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring[h.ringPos] = buf
	h.ringPos = (h.ringPos + 1) % audioPrerollChunks
	for ch := range h.clients {
		select {
		case ch <- buf:
		default:
		}
	}
}

func (h *audioBroadcastHub) hasListeners() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

// ---------------------------------------------------------------------------
// instance — one UberSDR channel connection
// ---------------------------------------------------------------------------

type instance struct {
	freqHz      int // tuned frequency in Hz
	carrierHz   int // carrier offset (0 for direct tuning)
	audioMode   string
	label       string // e.g. "14230000_usb"
	bandwidthHz int    // filter bandwidth in Hz; 0 = server default

	ubersdrURL string
	password   string
	sessionID  string

	audioHub *audioBroadcastHub
	fftHub   *fftBroadcastHub
	snrAccum *snrAccumulator

	mu            sync.Mutex
	running       bool
	startedAt     time.Time
	reconnections int
	status        string // "running" | "reconnecting" | "stopped"

	loopCancel context.CancelFunc

	// Live stream format — set once the first packet arrives.
	streamMu         sync.RWMutex
	streamSampleRate int
	streamChannels   int

	// AudioCh delivers decoded mono S16LE PCM chunks to the caller.
	AudioCh chan []byte
}

func newInstance(freqHz, carrierHz int, audioMode, ubersdrURL, password, labelOverride string, bandwidthHz int) *instance {
	label := labelOverride
	if label == "" {
		label = fmt.Sprintf("%d_%s", freqHz, audioMode)
	}
	dialHz := freqHz - carrierHz
	if carrierHz != 0 {
		log.Printf("[%s] published freq %d Hz, carrier offset %d Hz → dial freq %d Hz (%s)",
			label, freqHz, carrierHz, dialHz, audioMode)
	} else {
		log.Printf("[%s] freq %d Hz (%s)", label, freqHz, audioMode)
	}
	return &instance{
		freqHz:      freqHz,
		carrierHz:   carrierHz,
		audioMode:   audioMode,
		label:       label,
		bandwidthHz: bandwidthHz,
		ubersdrURL:  ubersdrURL,
		password:    password,
		sessionID:   uuid.New().String(),
		audioHub:    newAudioBroadcastHub(),
		fftHub:      newFFTBroadcastHub(),
		snrAccum:    &snrAccumulator{},
		status:      "stopped",
		AudioCh:     make(chan []byte, 256),
	}
}

// setBandwidth updates the filter bandwidth. Takes effect on next reconnect.
func (inst *instance) setBandwidth(hz int) {
	inst.mu.Lock()
	inst.bandwidthHz = hz
	inst.mu.Unlock()
}

// getBandwidth returns the current filter bandwidth setting.
func (inst *instance) getBandwidth() int {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.bandwidthHz
}

// DrainSNR returns accumulated SNR stats since the last call and resets the accumulator.
func (inst *instance) DrainSNR() SNRStats {
	return inst.snrAccum.drain()
}

// PeekSNR returns current SNR stats without resetting.
func (inst *instance) PeekSNR() SNRStats {
	return inst.snrAccum.peek()
}

func (inst *instance) httpBase() string {
	u, _ := url.Parse(inst.ubersdrURL)
	scheme := u.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, u.Host)
}

// bandwidthParams returns the bandwidthLow and bandwidthHigh values (in Hz)
// for the given mode and bandwidth setting.
//
// UberSDR convention:
//   - USB: both values positive  (low=+300, high=+bwHz)
//   - LSB: both values negative  (low=-bwHz, high=-300)
//   - AM/FM/NFM/SAM/CW: symmetric (low=-half, high=+half)
func bandwidthParams(mode string, bwHz int) (low, high int) {
	if bwHz <= 0 {
		return 0, 0 // 0 means "don't send" — server uses its default
	}
	switch strings.ToLower(mode) {
	case "usb":
		// Passband: +300 Hz to +bwHz (lower edge fixed at 300 Hz)
		low = 300
		high = bwHz
		if high < low {
			high = low + 1
		}
	case "lsb":
		// Mirror of USB: passband -bwHz to -300 Hz
		low = -bwHz
		high = -300
		if low > high {
			low = high - 1
		}
	case "cw":
		// Symmetric narrow filter
		half := bwHz / 2
		low = -half
		high = half
	default:
		// AM, FM, NFM, SAM — symmetric
		half := bwHz / 2
		low = -half
		high = half
	}
	return low, high
}

func (inst *instance) wsURL() string {
	u, _ := url.Parse(inst.ubersdrURL)
	wsScheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		wsScheme = "wss"
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/ws"
	}
	dialHz := inst.freqHz - inst.carrierHz
	q := url.Values{}
	q.Set("frequency", fmt.Sprintf("%d", dialHz))
	q.Set("mode", inst.audioMode)
	q.Set("format", "pcm-zstd")
	q.Set("version", "2")
	q.Set("user_session_id", inst.sessionID)
	if inst.password != "" {
		q.Set("password", inst.password)
	}
	// Send bandwidth parameters when a non-default value is configured.
	inst.mu.Lock()
	bwHz := inst.bandwidthHz
	inst.mu.Unlock()
	if bwHz > 0 {
		low, high := bandwidthParams(inst.audioMode, bwHz)
		q.Set("bandwidthLow", fmt.Sprintf("%d", low))
		q.Set("bandwidthHigh", fmt.Sprintf("%d", high))
	}
	return fmt.Sprintf("%s://%s%s?%s", wsScheme, u.Host, path, q.Encode())
}

func (inst *instance) checkConnection() (bool, error) {
	endpoint := inst.httpBase() + "/connection"
	body, _ := json.Marshal(connectionCheckRequest{
		UserSessionID: inst.sessionID,
		Password:      inst.password,
	})
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ubersdr_airsplice/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[%s] connection check failed (%v), attempting anyway", inst.label, err)
		return true, nil
	}
	defer resp.Body.Close()

	var cr connectionCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return false, fmt.Errorf("decode /connection response: %w", err)
	}
	if !cr.Allowed {
		return false, fmt.Errorf("server rejected connection: %s", cr.Reason)
	}
	log.Printf("[%s] connection allowed (IP: %s, bypassed: %v, max session: %ds)",
		inst.label, cr.ClientIP, cr.Bypassed, cr.MaxSessionTime)
	return true, nil
}

// runOnce performs one full connect → stream → disconnect cycle.
// Returns true if the caller should reconnect.
func (inst *instance) runOnce(ctx context.Context) (reconnect bool) {
	inst.mu.Lock()
	inst.sessionID = uuid.New().String()
	inst.mu.Unlock()

	allowed, err := inst.checkConnection()
	if err != nil {
		log.Printf("[%s] error: %v", inst.label, err)
		return true
	}
	if !allowed {
		return false
	}

	wsAddr := inst.wsURL()
	log.Printf("[%s] connecting to %s", inst.label, wsAddr)

	hdr := http.Header{}
	hdr.Set("User-Agent", "ubersdr_airsplice/1.0")
	conn, _, err := wsDialer.Dial(wsAddr, hdr)
	if err != nil {
		log.Printf("[%s] websocket dial: %v", inst.label, err)
		return true
	}
	defer conn.Close()

	log.Printf("[%s] connected — freq=%d Hz, mode=%s", inst.label, inst.freqHz, inst.audioMode)

	dec, err := newPCMDecoder()
	if err != nil {
		log.Printf("[%s] decoder init: %v", inst.label, err)
		return false
	}
	defer dec.close()

	// Keepalive goroutine
	localCtx, localCancel := context.WithCancel(ctx)
	defer localCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-localCtx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
					log.Printf("[%s] keepalive: %v", inst.label, err)
					return
				}
			}
		}
	}()

	// Context-cancellation watcher
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-localCtx.Done():
		}
	}()

	var totalBytes atomic.Int64
	var totalPackets atomic.Int64

	firstPacket := true
	var instFFT *audioFFT // lazily created once sample rate is known

	inst.mu.Lock()
	inst.status = "running"
	inst.startedAt = time.Now()
	inst.mu.Unlock()

	for {
		inst.mu.Lock()
		running := inst.running
		inst.mu.Unlock()
		if !running {
			return false
		}

		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("[%s] context cancelled — exiting runOnce without reconnect", inst.label)
				return false
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[%s] server closed connection", inst.label)
			} else {
				log.Printf("[%s] read error: %v", inst.label, err)
			}
			return true
		}

		switch msgType {
		case websocket.BinaryMessage:
			pkt, err := dec.decode(msg, true /* pcm-zstd */)
			if err != nil {
				log.Printf("[%s] decode: %v", inst.label, err)
				continue
			}
			if len(pkt.pcm) == 0 {
				continue
			}
			if firstPacket {
				log.Printf("[%s] receiving audio: %d Hz, %d channel(s)", inst.label, pkt.sampleRate, pkt.channels)
				firstPacket = false
				inst.streamMu.Lock()
				inst.streamSampleRate = pkt.sampleRate
				inst.streamChannels = pkt.channels
				inst.streamMu.Unlock()
			}

			// Accumulate SNR from v2 full-header packets.
			// snrAccum.add() already silently drops NaN/Inf values, so just
			// feed every packet — the server may send NaN on early packets
			// before the demodulator has a valid measurement.
			if pkt.hasSigInfo {
				inst.snrAccum.add(pkt.basebandDBFS, pkt.noiseDBFS)
			}

			// Downmix stereo (wfm) to mono
			pcmData := pkt.pcm
			if pkt.channels == 2 {
				pcmData = downmixStereoToMono(pcmData)
			}

			totalBytes.Add(int64(len(pcmData)))
			totalPackets.Add(1)

			// Deliver to caller via AudioCh (non-blocking drop if full)
			select {
			case inst.AudioCh <- pcmData:
			default:
				log.Printf("[%s] AudioCh full, dropping chunk", inst.label)
			}

			// Tee to audio preview listeners
			if inst.audioHub.hasListeners() {
				inst.audioHub.broadcast(pcmData)
			}

			// Compute FFT and broadcast magnitude frames to spectrum listeners.
			if inst.fftHub.hasListeners() {
				if instFFT == nil {
					instFFT = newAudioFFT(pkt.sampleRate)
				}
				if frame := instFFT.push(pcmData); frame != nil {
					if data, err := json.Marshal(frame); err == nil {
						inst.fftHub.broadcast(data)
					}
				}
			}

		case websocket.TextMessage:
			var m wsMessage
			if err := json.Unmarshal(msg, &m); err != nil {
				log.Printf("[%s] json parse: %v", inst.label, err)
				continue
			}
			switch m.Type {
			case "error":
				log.Printf("[%s] server error: %s", inst.label, m.Error)
				return true
			case "status":
				log.Printf("[%s] status: session=%s freq=%d mode=%s",
					inst.label, m.SessionID, m.Frequency, m.Mode)
			case "pong":
				// keepalive ack — ignore
			}
		}
	}
}

// start runs the instance loop with exponential-backoff reconnect.
func (inst *instance) start(ctx context.Context) {
	inst.mu.Lock()
	inst.running = true
	inst.status = "reconnecting"
	inst.mu.Unlock()

	retries := 0
	maxBackoff := 60 * time.Second

	for {
		inst.mu.Lock()
		running := inst.running
		inst.mu.Unlock()
		if !running {
			break
		}

		select {
		case <-ctx.Done():
			return
		default:
		}

		inst.mu.Lock()
		inst.status = "reconnecting"
		inst.mu.Unlock()

		reconnect := inst.runOnce(ctx)

		inst.mu.Lock()
		running = inst.running
		inst.mu.Unlock()

		if !reconnect || !running {
			break
		}

		select {
		case <-ctx.Done():
			return
		default:
		}

		retries++
		backoff := time.Duration(1<<uint(retries)) * time.Second
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		inst.mu.Lock()
		inst.reconnections++
		inst.mu.Unlock()
		log.Printf("[%s] reconnecting in %.0fs (attempt %d)…", inst.label, backoff.Seconds(), retries)

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}

	inst.mu.Lock()
	running := inst.running
	inst.mu.Unlock()
	if !running {
		inst.mu.Lock()
		inst.status = "stopped"
		inst.mu.Unlock()
		log.Printf("[%s] stopped", inst.label)
	}
}

// stop signals the instance to stop after the current connection ends.
func (inst *instance) stop() {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.running = false
}

// statusSnapshot returns a copy of the instance's current status fields.
func (inst *instance) statusSnapshot() map[string]interface{} {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.streamMu.RLock()
	sr := inst.streamSampleRate
	ch := inst.streamChannels
	inst.streamMu.RUnlock()
	snr := inst.snrAccum.peek()
	return map[string]interface{}{
		"freq_hz":       inst.freqHz,
		"audio_mode":    inst.audioMode,
		"label":         inst.label,
		"status":        inst.status,
		"started_at":    inst.startedAt,
		"reconnections": inst.reconnections,
		"sample_rate":   sr,
		"channels":      ch,
		"snr":           snr,
		"bandwidth_hz":  inst.bandwidthHz,
	}
}
