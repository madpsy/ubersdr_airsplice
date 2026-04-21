// web.go — HTTP server: embedded static files, REST API, SSE live feed,
// audio preview streaming, per-channel status.
package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Session store — in-memory set of valid session tokens
// ---------------------------------------------------------------------------

type sessionStore struct {
	mu     sync.RWMutex
	tokens map[string]struct{}
}

func newSessionStore() *sessionStore {
	return &sessionStore{tokens: make(map[string]struct{})}
}

func (s *sessionStore) create() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("session token generation failed: " + err.Error())
	}
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = struct{}{}
	s.mu.Unlock()
	return tok
}

func (s *sessionStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.RLock()
	_, ok := s.tokens[tok]
	s.mu.RUnlock()
	return ok
}

const sessionCookieName = "ui_session"

func requiresAuth(w http.ResponseWriter, r *http.Request, uiPassword string, sessions *sessionStore) bool {
	if uiPassword == "" {
		http.Error(w, `{"error":"write actions are disabled — set UI_PASSWORD to enable them"}`, http.StatusForbidden)
		return false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || !sessions.valid(cookie.Value) {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

//go:embed static/*
var staticFiles embed.FS

// ---------------------------------------------------------------------------
// sseHub — fan-out of SSE events to browser clients
// ---------------------------------------------------------------------------

type sseEvent struct {
	Event string      `json:"event"`
	Data  interface{} `json:"data"`
}

type sseClient struct {
	ch    chan sseEvent
	label string
	done  chan struct{}
}

type sseHub struct {
	mu      sync.Mutex
	clients map[*sseClient]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{clients: make(map[*sseClient]struct{})}
}

func (h *sseHub) subscribe(label string) *sseClient {
	c := &sseClient{
		ch:    make(chan sseEvent, 64),
		label: label,
		done:  make(chan struct{}),
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *sseHub) unsubscribe(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	close(c.done)
}

func labelOfEvent(data interface{}) string {
	switch v := data.(type) {
	case map[string]interface{}:
		if lv, ok := v["label"].(string); ok {
			return lv
		}
	case map[string]string:
		return v["label"]
	case *recordingRecord:
		return v.Label
	}
	return ""
}

func (h *sseHub) broadcast(ev sseEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	evLabel := labelOfEvent(ev.Data)
	for c := range h.clients {
		if c.label != "" && evLabel != "" && evLabel != c.label {
			continue
		}
		select {
		case c.ch <- ev:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// WAV streaming header helpers (for live audio preview)
// ---------------------------------------------------------------------------

func writeStreamingWAVHeader(w http.ResponseWriter, sampleRate, channels int) {
	const maxSize = 0x7FFFFFFF
	bitsPerSample := 16
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataSize := maxSize - 36

	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	putU32LE(hdr[4:], uint32(maxSize))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	putU32LE(hdr[16:], 16)
	putU16LE(hdr[20:], 1) // PCM
	putU16LE(hdr[22:], uint16(channels))
	putU32LE(hdr[24:], uint32(sampleRate))
	putU32LE(hdr[28:], uint32(byteRate))
	putU16LE(hdr[32:], uint16(blockAlign))
	putU16LE(hdr[34:], uint16(bitsPerSample))
	copy(hdr[36:40], "data")
	putU32LE(hdr[40:], uint32(dataSize))

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(hdr)
}

func putU32LE(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func putU16LE(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

// ---------------------------------------------------------------------------
// startHTTPServer wires all routes and starts listening.
// ---------------------------------------------------------------------------

func startHTTPServer(addr string, store *recordingStore, hub *sseHub, channels []*recChannel, uiPassword string) error {
	sessions := newSessionStore()
	mux := http.NewServeMux()

	// -----------------------------------------------------------------------
	// Auth endpoints
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		configured := uiPassword != ""
		authed := false
		if configured {
			if cookie, err := r.Cookie(sessionCookieName); err == nil {
				authed = sessions.valid(cookie.Value)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"password_configured": configured,
			"authenticated":       authed,
		})
	})

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if uiPassword == "" {
			http.Error(w, `{"error":"no password configured"}`, http.StatusForbidden)
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if body.Password != uiPassword {
			http.Error(w, `{"error":"incorrect password"}`, http.StatusUnauthorized)
			return
		}
		tok := sessions.create()
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})

	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})

	// index.html served as a Go template so BASE_PATH can be injected.
	indexTmpl, indexTmplErr := func() (*template.Template, error) {
		data, err := staticFiles.ReadFile("static/index.html")
		if err != nil {
			return nil, err
		}
		return template.New("index").Parse(string(data))
	}()

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}

	basePath := func(r *http.Request) string {
		return strings.TrimRight(r.Header.Get("X-Forwarded-Prefix"), "/")
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		if indexTmplErr != nil {
			http.Error(w, "template error: "+indexTmplErr.Error(), http.StatusInternalServerError)
			return
		}
		bp := basePath(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		indexTmpl.Execute(w, map[string]string{"BasePath": bp}) //nolint:errcheck
	})

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Serve saved recordings for download.
	mux.Handle("/recordings/", http.StripPrefix("/recordings/", http.FileServer(http.Dir(store.outputDir))))

	// -----------------------------------------------------------------------
	// GET /api/recordings?label=&limit=50&offset=0
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/recordings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		label := q.Get("label")
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		offset, _ := strconv.Atoi(q.Get("offset"))

		recs := store.list(label, limit, offset)
		payload := map[string]interface{}{
			"recordings": recs,
			"count":      len(recs),
		}
		data, err := json.Marshal(payload)
		if err != nil {
			http.Error(w, "json encode error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})

	// -----------------------------------------------------------------------
	// DELETE /api/recordings/{id}
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/recordings/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requiresAuth(w, r, uiPassword, sessions) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/recordings/")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if err := store.delete(id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		hub.broadcast(sseEvent{Event: "recording_deleted", Data: map[string]string{"id": id}})
		writeJSON(w, map[string]string{"status": "deleted", "id": id})
	})

	// -----------------------------------------------------------------------
	// GET /api/channels — list all configured channels and their status
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/channels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var out []map[string]interface{}
		for _, ch := range channels {
			out = append(out, ch.statusSnapshot())
		}
		writeJSON(w, out)
	})

	// -----------------------------------------------------------------------
	// GET /api/live — SSE stream of recording_started / recording_saved / recording_deleted
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/live", func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("label")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		client := hub.subscribe(label)
		defer hub.unsubscribe(client)

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprintf(w, ": heartbeat\n\n")
				flusher.Flush()
			case ev, ok := <-client.ch:
				if !ok {
					return
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, data)
				flusher.Flush()
			}
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/fft?label= — SSE stream of FFT magnitude frames
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/fft", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		label := r.URL.Query().Get("label")
		var inst *instance
		for _, ch := range channels {
			if ch.label == label {
				inst = ch.inst
				break
			}
		}
		if inst == nil {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		fftCh := inst.fftHub.subscribe()
		defer inst.fftHub.unsubscribe(fftCh)

		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case data, ok := <-fftCh:
				if !ok {
					return
				}
				fmt.Fprintf(w, "event: fft\ndata: %s\n\n", data)
				flusher.Flush()
			case <-ticker.C:
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/audio/preview?label= — streaming WAV audio preview
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/audio/preview", func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("label")
		var inst *instance
		for _, ch := range channels {
			if ch.label == label {
				inst = ch.inst
				break
			}
		}
		if inst == nil {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}

		inst.streamMu.RLock()
		sr := inst.streamSampleRate
		inst.streamMu.RUnlock()
		if sr == 0 {
			sr = 8000
		}

		writeStreamingWAVHeader(w, sr, 1)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}

		audioCh := inst.audioHub.subscribe()
		defer inst.audioHub.unsubscribe(audioCh)

		for {
			select {
			case <-r.Context().Done():
				return
			case chunk, ok := <-audioCh:
				if !ok {
					return
				}
				if _, err := w.Write(chunk); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/snr?label= — SSE stream of SNR stats (every 500 ms)
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/snr", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		label := r.URL.Query().Get("label")
		var inst *instance
		for _, ch := range channels {
			if ch.label == label {
				inst = ch.inst
				break
			}
		}
		if inst == nil {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				stats := inst.snrAccum.peek()
				data, err := json.Marshal(stats)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: snr\ndata: %s\n\n", data)
				flusher.Flush()
			}
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/status — overall server status
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		store.mu.RLock()
		total := len(store.records)
		store.mu.RUnlock()

		var chStatus []map[string]interface{}
		for _, ch := range channels {
			chStatus = append(chStatus, ch.statusSnapshot())
		}

		writeJSON(w, map[string]interface{}{
			"total_recordings": total,
			"channels":         chStatus,
		})
	})

	log.Printf("[web] listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

// writeJSON encodes v as JSON and writes it to w.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[web] json encode: %v", err)
	}
}
