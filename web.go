// web.go — HTTP server: embedded static files, REST API, SSE live feed,
// audio preview streaming, per-channel status.
package main

import (
	"compress/gzip"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// gzip middleware — transparently compress compressible responses
// ---------------------------------------------------------------------------

// noopCloser wraps an io.Writer so it satisfies io.WriteCloser without
// actually closing anything (used when the underlying writer is not a Closer).
type noopCloser struct{ io.Writer }

func (noopCloser) Close() error { return nil }

// gzipResponseWriter wraps http.ResponseWriter and compresses the body with
// gzip when the client advertises Accept-Encoding: gzip.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          io.WriteCloser
	wroteHeader bool
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if !g.wroteHeader {
		g.wroteHeader = true
		g.ResponseWriter.WriteHeader(code)
	}
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	return g.gz.Write(b)
}

// Flush satisfies http.Flusher: flush the gzip stream then the underlying
// response writer (if it also implements Flusher).
func (g *gzipResponseWriter) Flush() {
	// gzip.Writer does not implement Flush directly; Close would finalise the
	// stream, so we just flush the underlying transport.
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// gzipMiddleware wraps handler h and compresses responses for clients that
// send Accept-Encoding: gzip, except for streaming/WebSocket paths where
// buffering would break the protocol.
func gzipMiddleware(h http.Handler) http.Handler {
	// Paths (prefix-matched) that must NOT be compressed.
	skipPrefixes := []string{
		"/api/snr",           // SSE streams (/api/snr and /api/snr/all)
		"/api/audio/preview", // streaming WAV
		"/api/live/",         // WebSocket upgrade
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip if client doesn't accept gzip.
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.ServeHTTP(w, r)
			return
		}
		// Skip streaming / WebSocket paths.
		for _, pfx := range skipPrefixes {
			if strings.HasPrefix(r.URL.Path, pfx) {
				h.ServeHTTP(w, r)
				return
			}
		}
		// Wrap the response writer.
		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			h.ServeHTTP(w, r)
			return
		}
		defer gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length") // length is unknown after compression
		grw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		h.ServeHTTP(grw, r)
	})
}

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

func startHTTPServer(addr string, store *recordingStore, hub *sseHub, mgr *channelManager, uiPassword string, rc *retentionConfig, retentionCfgPath string, qc *quotaConfig, quotaCfgPath string) error {
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
	// GET  /api/channels        — list all channels and their status
	// POST /api/channels        — add a new channel (auth required)
	//                             body: {"freq_hz": 7880000, "mode": "usb"}
	// DELETE /api/channels/{label} — remove a channel (auth required)
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/channels", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			channels := mgr.list()
			var out []map[string]interface{}
			for _, ch := range channels {
				out = append(out, ch.statusSnapshot())
			}
			writeJSON(w, out)

		case http.MethodPost:
			if !requiresAuth(w, r, uiPassword, sessions) {
				return
			}
			var body struct {
				FreqHz      int               `json:"freq_hz"`
				Mode        string            `json:"mode"`
				Name        string            `json:"name"`         // optional user-defined label
				SmartRecord SmartRecordConfig `json:"smart_record"` // optional VOX gate config
				Schedule    ScheduleConfig    `json:"schedule"`     // optional time-based schedule
				BandwidthHz int               `json:"bandwidth_hz"` // optional filter bandwidth
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			if body.FreqHz <= 0 {
				http.Error(w, `{"error":"freq_hz required"}`, http.StatusBadRequest)
				return
			}
			if body.Mode == "" {
				http.Error(w, `{"error":"mode required"}`, http.StatusBadRequest)
				return
			}
			if body.Schedule.Enabled {
				if errMsg := body.Schedule.Validate(); errMsg != "" {
					http.Error(w, fmt.Sprintf(`{"error":"schedule: %s"}`, errMsg), http.StatusBadRequest)
					return
				}
			}
			ch, err := mgr.add(body.FreqHz, body.Mode, body.Name, "", body.SmartRecord, body.Schedule, body.BandwidthHz)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusConflict)
				return
			}
			mgr.save()
			hub.broadcast(sseEvent{
				Event: "channel_added",
				Data:  map[string]interface{}{"label": ch.label, "freq_hz": body.FreqHz, "mode": body.Mode},
			})
			writeJSON(w, ch.statusSnapshot())

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/channels/", func(w http.ResponseWriter, r *http.Request) {
		label := strings.TrimPrefix(r.URL.Path, "/api/channels/")
		if label == "" {
			http.Error(w, "missing label", http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodDelete:
			if !requiresAuth(w, r, uiPassword, sessions) {
				return
			}
			// Capture the channel_id before removal so the SSE event can carry it.
			removedChannelID := ""
			for _, ch := range mgr.list() {
				if ch.label == label {
					removedChannelID = ch.channelID
					break
				}
			}
			if err := mgr.remove(label); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			mgr.save()
			hub.broadcast(sseEvent{
				Event: "channel_removed",
				Data:  map[string]interface{}{"label": label, "channel_id": removedChannelID},
			})
			writeJSON(w, map[string]string{"status": "removed", "label": label})

		case http.MethodPatch:
			// PATCH /api/channels/{label} — rename, update smart-record config, schedule, bandwidth, and/or quota.
			if !requiresAuth(w, r, uiPassword, sessions) {
				return
			}
			var body struct {
				Name        *string            `json:"name"`
				SmartRecord *SmartRecordConfig `json:"smart_record"`
				Schedule    *ScheduleConfig    `json:"schedule"`
				BandwidthHz *int               `json:"bandwidth_hz"`
				MaxMB       *int64             `json:"max_mb"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if body.Name == nil && body.SmartRecord == nil && body.Schedule == nil && body.BandwidthHz == nil && body.MaxMB == nil {
				http.Error(w, `{"error":"name, smart_record, schedule, bandwidth_hz, or max_mb required"}`, http.StatusBadRequest)
				return
			}

			// Apply rename if requested.
			newLabel := label
			if body.Name != nil && *body.Name != "" {
				var err error
				newLabel, err = mgr.rename(label, *body.Name)
				if err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusConflict)
					return
				}
				hub.broadcast(sseEvent{
					Event: "channel_renamed",
					Data:  map[string]string{"old_label": label, "new_label": newLabel},
				})
			}

			// Apply smart-record config if provided.
			if body.SmartRecord != nil {
				if err := mgr.setSmartRecord(newLabel, *body.SmartRecord); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
					return
				}
				hub.broadcast(sseEvent{
					Event: "channel_updated",
					Data:  map[string]interface{}{"label": newLabel, "smart_record": *body.SmartRecord},
				})
			}

			// Apply schedule if provided.
			if body.Schedule != nil {
				if body.Schedule.Enabled {
					if errMsg := body.Schedule.Validate(); errMsg != "" {
						http.Error(w, fmt.Sprintf(`{"error":"schedule: %s"}`, errMsg), http.StatusBadRequest)
						return
					}
				}
				if err := mgr.setSchedule(newLabel, *body.Schedule); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
					return
				}
				hub.broadcast(sseEvent{
					Event: "channel_updated",
					Data:  map[string]interface{}{"label": newLabel, "schedule": *body.Schedule},
				})
			}

			// Apply bandwidth if provided.
			if body.BandwidthHz != nil {
				if err := mgr.setBandwidth(newLabel, *body.BandwidthHz); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
					return
				}
				hub.broadcast(sseEvent{
					Event: "channel_updated",
					Data:  map[string]interface{}{"label": newLabel, "bandwidth_hz": *body.BandwidthHz},
				})
			}

			// Apply per-channel storage quota if provided.
			if body.MaxMB != nil {
				if *body.MaxMB < 0 {
					http.Error(w, `{"error":"max_mb must be >= 0"}`, http.StatusBadRequest)
					return
				}
				qc.setForLabel(newLabel, *body.MaxMB)
				qc.save(quotaCfgPath)
				hub.broadcast(sseEvent{
					Event: "channel_updated",
					Data:  map[string]interface{}{"label": newLabel, "max_mb": *body.MaxMB},
				})
			}

			mgr.save()
			writeJSON(w, map[string]string{"status": "ok", "label": newLabel})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/active — list all currently-recording (in-progress) segments
	// GET /api/active/{label}/stream — serve the partially-written WAV so far
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/active", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var active []*activeSegmentInfo
		for _, ch := range mgr.list() {
			if snap := ch.liveSnapshot(); snap != nil {
				active = append(active, snap)
			}
		}
		writeJSON(w, active)
	})

	mux.HandleFunc("/api/active/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Path: /api/active/{label}/stream
		rest := strings.TrimPrefix(r.URL.Path, "/api/active/")
		parts := strings.SplitN(rest, "/", 2)
		label := parts[0]
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		if label == "" || action != "stream" {
			http.Error(w, "use /api/active/{label}/stream", http.StatusBadRequest)
			return
		}

		// Find the channel.
		var snap *activeSegmentInfo
		for _, ch := range mgr.list() {
			if ch.label == label {
				snap = ch.liveSnapshot()
				break
			}
		}
		if snap == nil {
			http.Error(w, "channel not found or not recording", http.StatusNotFound)
			return
		}

		// Open the WAV file for reading (the recorder is writing to it concurrently).
		f, err := os.Open(snap.Path)
		if err != nil {
			http.Error(w, "cannot open recording: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()

		// Write a corrected WAV header with the bytes written so far, then stream the PCM.
		writeStreamingWAVHeader(w, snap.SampleRate, snap.Channels)
		flusher, _ := w.(http.Flusher)

		// Skip the 44-byte placeholder header in the file.
		if _, err := f.Seek(44, 0); err != nil {
			return
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/dates?channel_id=  — list UTC calendar dates that have recordings
	//                               (newest first); optional channel UUID filter.
	// Response: {"dates": ["2026-04-22", "2026-04-21", ...]}
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/dates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		channelID := r.URL.Query().Get("channel_id")
		dates := store.availableDates(channelID)
		writeJSON(w, map[string]interface{}{"dates": dates})
	})

	// -----------------------------------------------------------------------
	// GET /api/sessions?label=&limit=20&offset=0&date=YYYY-MM-DD
	//     Returns sessions (groups of segments) newest-first.
	//     date defaults to today (UTC); pass date= (empty) for all dates.
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		label := q.Get("label")
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 20
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		groupByChannel := q.Get("group_by") == "channel"

		// date param: always required; default to today if absent or empty.
		// "All dates" is no longer supported — every request must be scoped to
		// a specific UTC calendar day to avoid unbounded result sets.
		date := time.Now().UTC().Format("2006-01-02") // default: today
		if d, ok := q["date"]; ok && d[0] != "" {
			date = d[0]
		}

		var sessions2 []*sessionSummary
		var total int
		if groupByChannel {
			// Filter by channel UUID when provided; label filter is handled by listSessions.
			channelID := q.Get("channel_id")
			sessions2, total = store.listByChannelID(channelID, limit, offset, date)
		} else {
			sessions2, total = store.listSessions(label, limit, offset, date)
		}
		writeJSON(w, map[string]interface{}{
			"sessions": sessions2,
			"total":    total,
			"limit":    limit,
			"offset":   offset,
			"date":     date,
		})
	})

	// -----------------------------------------------------------------------
	// GET /api/sessions/{id}/stream  — concatenated WAV stream for a session
	// GET /api/sessions/{id}         — session metadata JSON
	// DELETE /api/sessions/{id}      — delete all segments in a session (auth)
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		// Path: /api/sessions/{id}  or  /api/sessions/{id}/stream
		rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		parts := strings.SplitN(rest, "/", 2)
		sessionID := parts[0]
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		if sessionID == "" {
			http.Error(w, "missing session id", http.StatusBadRequest)
			return
		}

		// For telemetry the caller may pass ?date= to scope the session to a
		// specific day (so the timeline matches the date-filtered segment list).
		// For stream/delete we always use all dates so playback/deletion work.
		q := r.URL.Query()
		sessionDate := ""
		if action == "telemetry" {
			if d, ok := q["date"]; ok {
				sessionDate = d[0]
			}
		}
		ss := store.getSession(sessionID, sessionDate)

		// For telemetry on a live (not-yet-stored) session, fall back to the
		// active channel whose session_id matches.
		if ss == nil && action == "telemetry" {
			for _, ch := range mgr.list() {
				snap := ch.liveSnapshot()
				if snap == nil || snap.SessionID != sessionID {
					continue
				}
				// Build a minimal telemetry response from the in-progress .jsonl.
				// When sessionDate is set, only include points from that UTC date
				// so a channel that has been recording since yesterday doesn't
				// bleed yesterday's data into today's view.
				type telPoint struct {
					T         string   `json:"t"`
					SNR       SNRStats `json:"snr"`
					LevelDB   float32  `json:"level_dbfs"`
					SegIdx    int      `json:"seg_idx"`
					OffsetSec float64  `json:"offset_sec"`
				}
				var points []telPoint
				var refStart time.Time // wall-clock origin for offset_sec
				if snap.JsonlPath != "" {
					data, err := os.ReadFile(snap.JsonlPath)
					if err == nil {
						for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
							if line == "" {
								continue
							}
							var entry struct {
								T         string   `json:"t"`
								SNR       SNRStats `json:"snr"`
								LevelDBFS float32  `json:"level_dbfs"`
							}
							if err := json.Unmarshal([]byte(line), &entry); err != nil {
								continue
							}
							t, err := time.Parse(time.RFC3339, entry.T)
							if err != nil {
								continue
							}
							// Date-filter: skip points not on the requested date.
							if sessionDate != "" && t.UTC().Format("2006-01-02") != sessionDate {
								continue
							}
							// Use the first matching point as the reference start.
							if refStart.IsZero() {
								refStart = t
							}
							points = append(points, telPoint{
								T:         entry.T,
								SNR:       entry.SNR,
								LevelDB:   entry.LevelDBFS,
								SegIdx:    snap.SegmentIndex,
								OffsetSec: t.Sub(refStart).Seconds(),
							})
						}
					}
				}
				// If no date-filtered points, fall back to snap.StartedAt as origin.
				if refStart.IsZero() {
					refStart = snap.StartedAt
				}
				// totalDur: when points are date-filtered, base it on the filtered
				// span (last point offset + one interval) so a long-running segment
				// that started yesterday doesn't inflate the window to 14+ hours.
				var totalDur float64
				if len(points) > 0 {
					totalDur = points[len(points)-1].OffsetSec + 10
				} else {
					totalDur = snap.DurationSec
				}
				writeJSON(w, map[string]interface{}{
					"session_id":   sessionID,
					"started_at":   refStart,
					"duration_sec": totalDur,
					"points":       points,
				})
				return
			}
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		if ss == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		switch {
		case r.Method == http.MethodGet && action == "stream":
			// Stream all segments as a single concatenated WAV.
			if len(ss.Segments) == 0 {
				http.Error(w, "no segments", http.StatusNotFound)
				return
			}
			first := ss.Segments[0]
			writeStreamingWAVHeader(w, first.SampleRate, first.Channels)
			flusher, _ := w.(http.Flusher)
			for _, seg := range ss.Segments {
				path := filepath.Join(store.outputDir, seg.Filename)
				f, err := os.Open(path)
				if err != nil {
					log.Printf("[web] session stream: open %s: %v", path, err)
					continue
				}
				// Skip the 44-byte WAV header.
				if _, err := f.Seek(44, 0); err != nil {
					f.Close()
					continue
				}
				buf := make([]byte, 32*1024)
				for {
					n, err := f.Read(buf)
					if n > 0 {
						if _, werr := w.Write(buf[:n]); werr != nil {
							f.Close()
							return
						}
						if flusher != nil {
							flusher.Flush()
						}
					}
					if err != nil {
						break
					}
				}
				f.Close()
				select {
				case <-r.Context().Done():
					return
				default:
				}
			}

		case r.Method == http.MethodGet && action == "telemetry":
			// Read all .jsonl telemetry files for this session's segments and
			// return a merged, time-ordered array of SNR/level data points.
			type telPoint struct {
				T         string   `json:"t"`
				SNR       SNRStats `json:"snr"`
				LevelDB   float32  `json:"level_dbfs"`
				SegIdx    int      `json:"seg_idx"`
				OffsetSec float64  `json:"offset_sec"` // seconds from session start
			}
			sessStart := ss.StartedAt
			var points []telPoint
			for _, seg := range ss.Segments {
				base := strings.TrimSuffix(seg.Filename, filepath.Ext(seg.Filename))
				jsonlPath := filepath.Join(store.outputDir, base+".jsonl")
				data, err := os.ReadFile(jsonlPath)

				segPoints := 0
				if err == nil {
					for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
						if line == "" {
							continue
						}
						var entry struct {
							T         string   `json:"t"`
							SNR       SNRStats `json:"snr"`
							LevelDBFS float32  `json:"level_dbfs"`
						}
						if err := json.Unmarshal([]byte(line), &entry); err != nil {
							continue
						}
						t, err := time.Parse(time.RFC3339, entry.T)
						if err != nil {
							continue
						}
						points = append(points, telPoint{
							T:         entry.T,
							SNR:       entry.SNR,
							LevelDB:   entry.LevelDBFS,
							SegIdx:    seg.SegmentIndex,
							OffsetSec: t.Sub(sessStart).Seconds(),
						})
						segPoints++
					}
				}

				// If no .jsonl data exists for this segment (short segment, crash, etc.)
				// synthesise a single point from the segment's own SNR at its midpoint.
				if segPoints == 0 && seg.SNR.Count > 0 {
					midOffset := seg.StartedAt.Sub(sessStart).Seconds() + seg.DurationSec/2
					points = append(points, telPoint{
						T:         seg.StartedAt.UTC().Format(time.RFC3339),
						SNR:       seg.SNR,
						LevelDB:   float32(seg.SNR.BasebandAvg),
						SegIdx:    seg.SegmentIndex,
						OffsetSec: midOffset,
					})
				}
			}
			// Also append telemetry from the currently-recording (live) segment
			// for this channel, if any. The live segment is not yet in the store.
			// When sessionDate is set, only include points from that UTC date so
			// a long-running segment that started yesterday doesn't bleed into
			// today's view.
			for _, ch := range mgr.list() {
				snap := ch.liveSnapshot()
				if snap == nil || snap.ChannelID != sessionID {
					continue
				}
				if snap.JsonlPath == "" {
					break
				}
				data, err := os.ReadFile(snap.JsonlPath)
				if err != nil {
					break
				}
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					if line == "" {
						continue
					}
					var entry struct {
						T         string   `json:"t"`
						SNR       SNRStats `json:"snr"`
						LevelDBFS float32  `json:"level_dbfs"`
					}
					if err := json.Unmarshal([]byte(line), &entry); err != nil {
						continue
					}
					t, err := time.Parse(time.RFC3339, entry.T)
					if err != nil {
						continue
					}
					// Date-filter: skip points not on the requested date.
					if sessionDate != "" && t.UTC().Format("2006-01-02") != sessionDate {
						continue
					}
					points = append(points, telPoint{
						T:         entry.T,
						SNR:       entry.SNR,
						LevelDB:   entry.LevelDBFS,
						SegIdx:    snap.SegmentIndex,
						OffsetSec: t.Sub(sessStart).Seconds(),
					})
				}
				break
			}

			// Sort by time offset
			sort.Slice(points, func(i, j int) bool {
				return points[i].OffsetSec < points[j].OffsetSec
			})
			// Total duration = wall-clock span including live segment.
			// Add one telemetry interval (10 s) of padding so the last bar
			// has visible width on the canvas.
			totalDur := ss.DurationSec
			if len(points) > 0 {
				lastOff := points[len(points)-1].OffsetSec + 10
				if lastOff > totalDur {
					totalDur = lastOff
				}
			}
			writeJSON(w, map[string]interface{}{
				"session_id":   sessionID,
				"started_at":   ss.StartedAt,
				"duration_sec": totalDur,
				"points":       points,
			})

		case r.Method == http.MethodGet && action == "":
			writeJSON(w, ss)

		case r.Method == http.MethodDelete && action == "":
			if !requiresAuth(w, r, uiPassword, sessions) {
				return
			}
			// Delete all segments in the session.
			var deleteErrs []string
			for _, seg := range ss.Segments {
				if err := store.delete(seg.ID); err != nil {
					deleteErrs = append(deleteErrs, err.Error())
				} else {
					hub.broadcast(sseEvent{
						Event: "recording_deleted",
						Data:  map[string]string{"id": seg.ID},
					})
				}
			}
			if len(deleteErrs) > 0 {
				http.Error(w, strings.Join(deleteErrs, "; "), http.StatusInternalServerError)
				return
			}
			hub.broadcast(sseEvent{
				Event: "session_deleted",
				Data:  map[string]string{"session_id": sessionID},
			})
			writeJSON(w, map[string]string{"status": "deleted", "session_id": sessionID})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

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
	// DELETE /api/recordings/{id}        — delete a segment (auth required)
	// GET    /api/recordings/{id}/mp3    — transcode WAV→MP3 via ffmpeg
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/recordings/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/recordings/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}

		switch {
		case r.Method == http.MethodDelete && action == "":
			// DELETE /api/recordings/{id}
			if !requiresAuth(w, r, uiPassword, sessions) {
				return
			}
			if err := store.delete(id); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			hub.broadcast(sseEvent{Event: "recording_deleted", Data: map[string]string{"id": id}})
			writeJSON(w, map[string]string{"status": "deleted", "id": id})

		case r.Method == http.MethodGet && action == "mp3":
			// GET /api/recordings/{id}/mp3 — transcode WAV→MP3 on-the-fly via ffmpeg.
			// Resolve the recording record to get the filename.
			store.mu.RLock()
			var rec *recordingRecord
			for _, r2 := range store.records {
				if r2.ID == id {
					rec = r2
					break
				}
			}
			store.mu.RUnlock()

			if rec == nil {
				http.Error(w, "recording not found", http.StatusNotFound)
				return
			}

			wavPath := filepath.Join(store.outputDir, rec.Filename)

			// Build the download filename: same base name but .mp3 extension.
			base := strings.TrimSuffix(filepath.Base(rec.Filename), filepath.Ext(rec.Filename))
			mp3Name := base + ".mp3"

			// Run lame: read WAV from file, write MP3 to stdout.
			// -V 4 ≈ 165 kbps VBR — good quality, small file.
			// --silent suppresses the progress output on stderr.
			// The trailing "-" tells lame to write to stdout.
			cmd := exec.CommandContext(r.Context(),
				"lame", "--silent", "-V", "4", wavPath, "-",
			)
			// Discard lame's stderr so it doesn't leak into the response.
			cmd.Stderr = nil

			mp3Data, err := cmd.Output()
			if err != nil {
				log.Printf("[web] ffmpeg mp3 encode %s: %v", id, err)
				http.Error(w, "MP3 encoding failed: "+err.Error(), http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Content-Disposition", `attachment; filename="`+mp3Name+`"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(mp3Data)))
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mp3Data)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/live — SSE stream of events
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
		for _, ch := range mgr.list() {
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
		for _, ch := range mgr.list() {
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
	// GET /api/live/{label}/ws — real-time WebSocket audio stream
	//
	// Protocol:
	//   1. Server sends a JSON text frame: {"sample_rate":8000,"channels":1}
	//   2. Server sends binary frames: raw S16LE mono PCM chunks (no header)
	//   3. Client closes the connection to stop.
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/live/", func(w http.ResponseWriter, r *http.Request) {
		// Path: /api/live/{label}/ws
		rest := strings.TrimPrefix(r.URL.Path, "/api/live/")
		parts := strings.SplitN(rest, "/", 2)
		label := parts[0]
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		if label == "" || action != "ws" {
			http.Error(w, "use /api/live/{label}/ws", http.StatusBadRequest)
			return
		}

		var inst *instance
		for _, ch := range mgr.list() {
			if ch.label == label {
				inst = ch.inst
				break
			}
		}
		if inst == nil {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}

		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[live-ws] upgrade %s: %v", label, err)
			return
		}
		defer conn.Close()

		// Send stream info as first text frame.
		inst.streamMu.RLock()
		sr := inst.streamSampleRate
		inst.streamMu.RUnlock()
		if sr == 0 {
			sr = 8000
		}
		info, _ := json.Marshal(map[string]int{"sample_rate": sr, "channels": 1})
		if err := conn.WriteMessage(websocket.TextMessage, info); err != nil {
			return
		}

		audioCh := inst.audioHub.subscribe()
		defer inst.audioHub.unsubscribe(audioCh)

		// Drain incoming messages (client pings / close frames) in background.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		for {
			select {
			case <-r.Context().Done():
				return
			case chunk, ok := <-audioCh:
				if !ok {
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
					return
				}
			}
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/snr/all — SSE stream of SNR for ALL channels (every 200 ms)
	// Sends: event: snr\ndata: [{"label":"...","snr":{...}}, ...]\n\n
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/snr/all", func(w http.ResponseWriter, r *http.Request) {
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
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		type snrEntry struct {
			Label string   `json:"label"`
			SNR   SNRStats `json:"snr"`
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				channels := mgr.list()
				entries := make([]snrEntry, 0, len(channels))
				for _, ch := range channels {
					entries = append(entries, snrEntry{
						Label: ch.label,
						SNR:   ch.inst.snrAccum.peekLatest(2),
					})
				}
				data, err := json.Marshal(entries)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: snr\ndata: %s\n\n", data)
				flusher.Flush()
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
		for _, ch := range mgr.list() {
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
	// GET  /api/retention          — read current retention settings
	// POST /api/retention          — update retention settings (auth-gated)
	//
	// GET response:  {"default_hours":0,"channels":{"7880000_usb":48}}
	// POST body:     {"default_hours":0,"channels":{"7880000_usb":48}}
	//   OR per-channel shorthand: {"label":"7880000_usb","keep_hours":48}
	//
	// keep_hours / default_hours == 0 means keep forever.
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/retention", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			defH, chMap := rc.snapshot()
			writeJSON(w, map[string]interface{}{
				"default_hours": defH,
				"channels":      chMap,
			})

		case http.MethodPost:
			if !requiresAuth(w, r, uiPassword, sessions) {
				return
			}
			// Accept two shapes:
			//   {"label":"...", "keep_hours": N}  — set one channel
			//   {"default_hours": N, "channels": {...}}  — bulk update
			var raw map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			if labelRaw, ok := raw["label"]; ok {
				// Per-channel shorthand
				var label string
				var hours int
				if err := json.Unmarshal(labelRaw, &label); err != nil {
					http.Error(w, "invalid label", http.StatusBadRequest)
					return
				}
				if h, ok := raw["keep_hours"]; ok {
					if err := json.Unmarshal(h, &hours); err != nil {
						http.Error(w, "invalid keep_hours", http.StatusBadRequest)
						return
					}
				}
				if hours < 0 {
					http.Error(w, "keep_hours must be >= 0", http.StatusBadRequest)
					return
				}
				rc.setForLabel(label, hours)
				log.Printf("[retention] %s: keep_hours=%d", label, hours)
			} else {
				// Bulk update
				if h, ok := raw["default_hours"]; ok {
					var defH int
					if err := json.Unmarshal(h, &defH); err == nil && defH >= 0 {
						rc.setDefault(defH)
					}
				}
				if ch, ok := raw["channels"]; ok {
					var chMap map[string]int
					if err := json.Unmarshal(ch, &chMap); err == nil {
						for label, hours := range chMap {
							if hours >= 0 {
								rc.setForLabel(label, hours)
							}
						}
					}
				}
				log.Printf("[retention] bulk update applied")
			}
			rc.save(retentionCfgPath)
			defH, chMap := rc.snapshot()
			writeJSON(w, map[string]interface{}{
				"default_hours": defH,
				"channels":      chMap,
			})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// -----------------------------------------------------------------------
	// GET  /api/quota          — read current quota settings
	// POST /api/quota          — update quota settings (auth-gated)
	//
	// GET response:  {"overall_mb":20480,"channels":{"7880000_usb":1024}}
	// POST body (overall):      {"overall_mb": 20480}
	// POST body (per-channel):  {"label":"7880000_usb","max_mb":1024}
	// POST body (bulk):         {"overall_mb":20480,"channels":{"7880000_usb":1024}}
	//
	// max_mb / overall_mb == 0 means unlimited.
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/quota", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			overallMB, chMap := qc.snapshot()
			writeJSON(w, map[string]interface{}{
				"overall_mb": overallMB,
				"channels":   chMap,
			})

		case http.MethodPost:
			if !requiresAuth(w, r, uiPassword, sessions) {
				return
			}
			var raw map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			if labelRaw, ok := raw["label"]; ok {
				// Per-channel shorthand: {"label":"...", "max_mb": N}
				var label string
				var mb int64
				if err := json.Unmarshal(labelRaw, &label); err != nil {
					http.Error(w, "invalid label", http.StatusBadRequest)
					return
				}
				if m, ok := raw["max_mb"]; ok {
					if err := json.Unmarshal(m, &mb); err != nil {
						http.Error(w, "invalid max_mb", http.StatusBadRequest)
						return
					}
				}
				if mb < 0 {
					http.Error(w, "max_mb must be >= 0", http.StatusBadRequest)
					return
				}
				qc.setForLabel(label, mb)
				log.Printf("[quota] %s: max_mb=%d", label, mb)
			} else {
				// Bulk / overall update
				if m, ok := raw["overall_mb"]; ok {
					var mb int64
					if err := json.Unmarshal(m, &mb); err == nil && mb >= 0 {
						qc.setOverall(mb)
						log.Printf("[quota] overall_mb=%d", mb)
					}
				}
				if ch, ok := raw["channels"]; ok {
					var chMap map[string]int64
					if err := json.Unmarshal(ch, &chMap); err == nil {
						for label, mb := range chMap {
							if mb >= 0 {
								qc.setForLabel(label, mb)
							}
						}
					}
				}
				log.Printf("[quota] bulk update applied")
			}
			qc.save(quotaCfgPath)
			overallMB, chMap := qc.snapshot()
			writeJSON(w, map[string]interface{}{
				"overall_mb": overallMB,
				"channels":   chMap,
			})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// -----------------------------------------------------------------------
	// GET /api/status — overall server status
	// -----------------------------------------------------------------------
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		store.mu.RLock()
		total := len(store.records)
		store.mu.RUnlock()

		channels := mgr.list()
		var chStatus []map[string]interface{}
		for _, ch := range channels {
			chStatus = append(chStatus, ch.statusSnapshot())
		}

		overallMB, chQuota := qc.snapshot()
		writeJSON(w, map[string]interface{}{
			"total_recordings": total,
			"channels":         chStatus,
			"quota": map[string]interface{}{
				"overall_mb": overallMB,
				"channels":   chQuota,
			},
		})
	})

	log.Printf("[web] listening on %s", addr)
	return http.ListenAndServe(addr, gzipMiddleware(mux))
}

// writeJSON encodes v as JSON and writes it to w.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[web] json encode: %v", err)
	}
}
