// Package api menyediakan permukaan HTTP copilotd.
//
// Semua endpoint versioned di /v1. Endpoint lama (/models, /stream) tetap
// dipertahankan sebagai alias supaya klien yang sudah ada tidak putus.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

// Server merutekan permintaan HTTP ke service Copilot.
type Server struct {
	svc    *runtime.Service
	apiKey string
}

// New menyiapkan server.
func New(svc *runtime.Service, apiKey string) *Server {
	return &Server{svc: svc, apiKey: apiKey}
}

// Handler membangun mux lengkap beserta middleware autentikasi.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health sengaja tanpa auth supaya bisa dipakai probe/systemd.
	mux.HandleFunc("GET /health", s.handleHealth)

	mux.HandleFunc("GET /v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	mux.HandleFunc("POST /v1/generate", s.auth(s.handleGenerate))
	mux.HandleFunc("POST /v1/stream", s.auth(s.handleStream))
	mux.HandleFunc("POST /v1/agent", s.auth(s.handleAgent))

	// Alias tanpa versi (kompatibilitas klien lama).
	mux.HandleFunc("GET /models", s.auth(s.handleModels))
	mux.HandleFunc("POST /stream", s.auth(s.handleStream))

	return mux
}

// auth mewajibkan header X-API-Key bila kunci dikonfigurasi.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" && r.Header.Get("X-API-Key") != s.apiKey {
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "API key salah"})
			return
		}
		next(w, r)
	}
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func decode(r *http.Request, dst any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(dst)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleStatus adalah pemeriksaan murah: tidak menyalakan runtime.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := s.svc.Config()
	auth := "github"
	if cfg.IsBYOK() {
		auth = "byok"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"accounts":          len(cfg.Tokens()),
		"model":             modelOrDefault(cfg.Model),
		"agent_model":       modelOrDefault(cfg.ModelFor("agent")),
		"auth":              auth,
		"provider_base_url": cfg.ProviderBaseURL,
		"local_only":        cfg.IsLocalProvider(),
		"max_concurrency":   cfg.MaxConcurrency,
	})
}

func modelOrDefault(model string) string {
	if model == "" {
		return "(runtime default)"
	}
	return model
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 90*time.Second)
	defer cancel()

	models, err := s.svc.Models(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{Error: err.Error()})
		return
	}

	type modelOut struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Multiplier *float64 `json:"multiplier,omitempty"`
	}
	ids := make([]string, 0, len(models))
	detail := make([]modelOut, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		ids = append(ids, m.ID)
		out := modelOut{ID: m.ID, Name: m.Name}
		if m.Billing != nil {
			out.Multiplier = m.Billing.Multiplier
		}
		detail = append(detail, out)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":  ids,
		"detail":  detail,
		"default": s.svc.Config().Model,
	})
}
