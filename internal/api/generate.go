package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

// generateRequest adalah bentuk permintaan non-streaming.
type generateRequest struct {
	Prompt     string  `json:"prompt"`
	System     string  `json:"system"`
	Model      string  `json:"model"`
	Tier       string  `json:"tier"`
	JSONMode   bool    `json:"json_mode"`
	Web        bool    `json:"web"`
	TimeoutSec float64 `json:"timeout_sec"`
}

func (g generateRequest) toRuntime() runtime.Request {
	return runtime.Request{
		Prompt:   strings.TrimSpace(g.Prompt),
		System:   g.System,
		Model:    g.Model,
		Tier:     g.Tier,
		JSONMode: g.JSONMode,
		Web:      g.Web,
		Timeout:  time.Duration(g.TimeoutSec * float64(time.Second)),
	}
}

// handleGenerate menjalankan satu giliran dan mengembalikan teks final saja.
func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var body generateRequest
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "JSON tidak valid"})
		return
	}
	req := body.toRuntime()
	if req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "prompt kosong"})
		return
	}

	ctx, cancel := contextWithTimeout(r, 0)
	defer cancel()

	var (
		mu    sync.Mutex
		text  string
		model string
	)
	err := s.svc.Stream(ctx, req, nil, nil, func(ev runtime.Event) {
		if ev.Type != "done" {
			return
		}
		mu.Lock()
		text, model = ev.Content, ev.Model
		mu.Unlock()
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "model": model})
}

// handleStream mengalirkan jawaban sebagai NDJSON.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	var body generateRequest
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "JSON tidak valid"})
		return
	}
	req := body.toRuntime()
	if req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "prompt kosong"})
		return
	}

	emit, flush, ok := startNDJSON(w)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, 0)
	defer cancel()

	if err := s.svc.Stream(ctx, req, nil, nil, emit); err != nil {
		emit(runtime.Event{Type: "error", Message: err.Error()})
	}
	flush()
}

// startNDJSON menyiapkan response streaming dan mengembalikan emitter yang
// aman dipanggil dari goroutine mana pun.
func startNDJSON(w http.ResponseWriter) (func(runtime.Event), func(), bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "streaming tidak didukung"})
		return nil, nil, false
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	var mu sync.Mutex
	emit := func(ev runtime.Event) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(ev)
		flusher.Flush()
	}
	flush := func() {
		mu.Lock()
		defer mu.Unlock()
		flusher.Flush()
	}
	return emit, flush, true
}

// contextWithTimeout membatasi umur permintaan. extra == 0 berarti mengikuti
// context bawaan request (service sudah menerapkan timeout-nya sendiri).
func contextWithTimeout(r *http.Request, extra time.Duration) (context.Context, context.CancelFunc) {
	if extra <= 0 {
		return context.WithCancel(r.Context())
	}
	return context.WithTimeout(r.Context(), extra)
}

var errNoToolResult = errors.New("hasil tool tidak diterima")
