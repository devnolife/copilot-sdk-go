// Command copilotd menjalankan Copilot sebagai layanan HTTP kecil.
//
// Backend riset berjalan di Python 3.10, sedangkan github-copilot-sdk versi
// baru mensyaratkan Python 3.11+. Daripada membangun ulang virtualenv 6.7 GB
// (torch, transformers, dsb.) hanya demi satu paket, Copilot dibungkus di sini
// memakai SDK Go — runtime CLI dan kredensial `copilot` yang sudah login
// dipakai apa adanya.
//
// Endpoint:
//
//	GET  /health          → {"ok":true}
//	GET  /models          → {"models":["auto","claude-opus-5",…]}
//	POST /stream          → NDJSON: {"type":"delta"|"done"|"error", …}
//
// Layanan ini sengaja hanya bind ke loopback dan meminta header X-API-Key
// bila COPILOTD_API_KEY di-set — persis pola yang dipakai backend lain.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

const (
	defaultAddr    = "127.0.0.1:8791"
	requestTimeout = 10 * time.Minute
	// Sesi Copilot berat; batasi berapa yang boleh hidup bersamaan.
	maxConcurrent = 4
)

// runtime membungkus satu CopilotClient yang dipakai bersama. Menyalakan
// runtime CLI butuh beberapa detik, jadi client-nya di-cache dan hanya
// dibangun ulang kalau gagal.
type runtime struct {
	mu     sync.Mutex
	client *copilot.Client
	slots  chan struct{}
}

func newRuntime() *runtime {
	return &runtime{slots: make(chan struct{}, maxConcurrent)}
}

func (r *runtime) get(ctx context.Context) (*copilot.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return r.client, nil
	}
	client := copilot.NewClient(&copilot.ClientOptions{LogLevel: "error"})
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("runtime copilot gagal start: %w", err)
	}
	r.client = client
	return client, nil
}

// reset membuang client yang rusak supaya permintaan berikutnya membangun ulang.
func (r *runtime) reset() {
	r.mu.Lock()
	client := r.client
	r.client = nil
	r.mu.Unlock()
	if client != nil {
		_ = client.Stop()
	}
}

func (r *runtime) acquire(ctx context.Context) (func(), error) {
	select {
	case r.slots <- struct{}{}:
		return func() { <-r.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type streamRequest struct {
	Prompt string `json:"prompt"`
	System string `json:"system"`
	Model  string `json:"model"`
	// Web membuka tool pencarian web milik runtime CLI.
	Web bool `json:"web"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (r *runtime) handleModels(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 60*time.Second)
	defer cancel()

	client, err := r.get(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		r.reset()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}

var webTools = []string{"web_search", "web_fetch"}

// noTools adalah nama tool yang pasti tidak ada di registry. Mengirimnya
// sebagai satu-satunya AvailableTools membuat semua tool bawaan (shell, edit,
// fetch) tidak bisa dipakai model.
const noTools = "__none__"

func (r *runtime) handleStream(w http.ResponseWriter, req *http.Request) {
	var body streamRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON tidak valid"})
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt kosong"})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming tidak didukung"})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), requestTimeout)
	defer cancel()

	release, err := r.acquire(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "server sibuk"})
		return
	}
	defer release()

	client, err := r.get(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	cfg := &copilot.SessionConfig{
		Model:     body.Model,
		Streaming: copilot.Bool(true),
	}
	if body.Web {
		cfg.AvailableTools = webTools
	} else {
		cfg.AvailableTools = []string{noTools}
	}
	if body.System != "" {
		cfg.SystemMessage = &copilot.SystemMessageConfig{Mode: "replace", Content: body.System}
	}

	session, err := client.CreateSession(ctx, cfg)
	if err != nil {
		r.reset()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer session.Disconnect()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	// Event datang dari goroutine milik SDK; serialisasi penulisan response.
	var writeMu sync.Mutex
	emit := func(payload map[string]any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = enc.Encode(payload)
		flusher.Flush()
	}

	var (
		once     sync.Once
		done     = make(chan struct{})
		finalMu  sync.Mutex
		final    string
		streamed bool
		failure  string
	)
	finish := func() { once.Do(func() { close(done) }) }

	unsubscribe := session.On(func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			if d.DeltaContent != "" {
				finalMu.Lock()
				streamed = true
				finalMu.Unlock()
				emit(map[string]any{"type": "delta", "content": d.DeltaContent})
			}
		case *copilot.AssistantMessageData:
			finalMu.Lock()
			final = d.Content
			finalMu.Unlock()
		case *copilot.SessionErrorData:
			finalMu.Lock()
			failure = d.Message
			finalMu.Unlock()
			finish()
		case *copilot.SessionIdleData:
			finish()
		}
	})
	defer unsubscribe()

	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: body.Prompt}); err != nil {
		emit(map[string]any{"type": "error", "message": err.Error()})
		return
	}

	select {
	case <-done:
	case <-ctx.Done():
		emit(map[string]any{"type": "error", "message": "Copilot tidak merespons tepat waktu."})
		return
	}

	finalMu.Lock()
	text, hadStream, errMsg := final, streamed, failure
	finalMu.Unlock()

	if errMsg != "" {
		emit(map[string]any{"type": "error", "message": errMsg})
		return
	}
	if text == "" && !hadStream {
		emit(map[string]any{"type": "error", "message": "Copilot mengembalikan jawaban kosong."})
		return
	}
	emit(map[string]any{"type": "done", "content": text})
}

// authorize memastikan pemanggil membawa API key yang benar (kalau di-set).
func authorize(next http.HandlerFunc, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if key != "" && req.Header.Get("X-API-Key") != key {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "API key salah"})
			return
		}
		next(w, req)
	}
}

func main() {
	addr := flag.String("addr", envOr("COPILOTD_ADDR", defaultAddr), "alamat listen")
	flag.Parse()

	key := os.Getenv("COPILOTD_API_KEY")
	rt := newRuntime()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /models", authorize(rt.handleModels, key))
	mux.HandleFunc("POST /stream", authorize(rt.handleStream, key))

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Stream chat bisa panjang; jangan putus di tengah jalan.
		WriteTimeout: requestTimeout + time.Minute,
	}

	log.Printf("copilotd listening on %s", *addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
