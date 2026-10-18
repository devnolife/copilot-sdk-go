// Package api menyediakan permukaan HTTP copilotd.
//
// Semua endpoint versioned di /v1. Endpoint lama (/models, /stream) tetap
// dipertahankan sebagai alias supaya klien yang sudah ada tidak putus.
package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	copilot "github.com/github/copilot-sdk/go"

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

	// Pool akun (admin revisi.studio lewat core).
	mux.HandleFunc("GET /v1/accounts", s.auth(s.handleAccounts))
	mux.HandleFunc("POST /v1/accounts", s.auth(s.handleAddAccount))
	mux.HandleFunc("POST /v1/accounts/refresh", s.auth(s.handleRefreshAccounts))
	mux.HandleFunc("PUT /v1/accounts/strategy", s.auth(s.handleSetStrategy))
	mux.HandleFunc("PATCH /v1/accounts/{id}", s.auth(s.handleUpdateAccount))
	mux.HandleFunc("DELETE /v1/accounts/{id}", s.auth(s.handleRemoveAccount))

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
		"ok":                      true,
		"accounts":                s.svc.AccountCount(),
		"account_pool":            s.svc.Accounts(),
		"pool_strategy":           s.svc.PoolStrategy(),
		"quota_min_percent":       cfg.QuotaMinPercent,
		"quota_check_sec":         int(cfg.QuotaCheckInterval / time.Second),
		"model":                   modelOrDefault(cfg.Model),
		"agent_model":             modelOrDefault(cfg.ModelFor("agent")),
		"auth":                    auth,
		"provider_base_url":       cfg.ProviderBaseURL,
		"local_only":              cfg.IsLocalProvider(),
		"max_concurrency":         cfg.MaxConcurrency,
		"managed_generation_v1":   true,
		"session_ai_credit_limit": true,
	})
}

func modelOrDefault(model string) string {
	if model == "" {
		return "(runtime default)"
	}
	return model
}

// promptTokens: anggaran token masukan tier bawaan (dari harga per tier; batas model bila tidak dilaporkan).
func promptTokens(m copilot.ModelInfo) int64 {
	if m.Billing != nil && m.Billing.TokenPrices != nil && m.Billing.TokenPrices.MaxPromptTokens != nil {
		return *m.Billing.TokenPrices.MaxPromptTokens
	}
	limits := m.Capabilities.Limits
	switch {
	case limits.MaxPromptTokens != nil:
		return int64(*limits.MaxPromptTokens)
	case limits.MaxContextWindowTokens != nil:
		return int64(*limits.MaxContextWindowTokens)
	}
	return 0
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
		ID                     string   `json:"id"`
		Name                   string   `json:"name"`
		Multiplier             *float64 `json:"multiplier,omitempty"`
		ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
		DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
		ContextTokens          int64    `json:"context_tokens,omitempty"`
		LongContextTokens      int64    `json:"long_context_tokens,omitempty"`
	}
	ids := make([]string, 0, len(models))
	detail := make([]modelOut, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		ids = append(ids, m.ID)
		out := modelOut{ID: m.ID, Name: m.Name, ContextTokens: promptTokens(m)}
		if m.Billing != nil {
			out.Multiplier = m.Billing.Multiplier
			if p := m.Billing.TokenPrices; p != nil && p.LongContext != nil && p.LongContext.MaxPromptTokens != nil {
				out.LongContextTokens = *p.LongContext.MaxPromptTokens
			}
		}
		if m.Capabilities.Supports.ReasoningEffort {
			for _, e := range runtime.ReasoningEfforts {
				if slices.Contains(m.SupportedReasoningEfforts, e) {
					out.ReasoningEfforts = append(out.ReasoningEfforts, e)
				}
			}
			if slices.Contains(out.ReasoningEfforts, m.DefaultReasoningEffort) {
				out.DefaultReasoningEffort = m.DefaultReasoningEffort
			}
		}
		detail = append(detail, out)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":  ids,
		"detail":  detail,
		"default": s.svc.Config().Model,
	})
}
