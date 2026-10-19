package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/devnolife/copilot-sdk-go/internal/racikan"
	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

// racikanRequest mengganti seluruh isian admin: field yang tidak dikirim atau null kembali ikut .env.
type racikanRequest struct {
	Override *racikan.Override `json:"override"`
	Actor    string            `json:"actor"`
}

// handleRacikan: GET /v1/config/models — model efektif tiap tier, bawaan .env, dan isian admin.
func (s *Server) handleRacikan(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Racikan())
}

// handleSetRacikan: PUT /v1/config/models — simpan racikan admin (berlaku tanpa restart).
func (s *Server) handleSetRacikan(w http.ResponseWriter, r *http.Request) {
	var body racikanRequest
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "body bukan JSON yang sah"})
		return
	}
	if body.Override == nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "override wajib diisi (objek kosong = semua ikut .env)"})
		return
	}
	actor := strings.TrimSpace(body.Actor)
	if actor == "" {
		actor = "admin"
	}
	if len(actor) > 200 {
		actor = actor[:200]
	}
	state, err := s.svc.SetRacikan(*body.Override, actor)
	switch {
	case errors.Is(err, runtime.ErrRacikanReadOnly):
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error()})
	case err != nil && body.Override.Validate() != nil:
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "racikan gagal disimpan: " + err.Error()})
	default:
		writeJSON(w, http.StatusOK, state)
	}
}
