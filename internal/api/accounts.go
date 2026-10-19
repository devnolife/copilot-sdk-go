package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

// Pengelolaan pool akun untuk admin revisi.studio (lewat core). Token hanya diterima di body POST /v1/accounts dan
// tidak pernah muncul di respons, pesan galat, maupun log.

const (
	// maxAccountBody membatasi body permintaan pengelolaan akun.
	maxAccountBody = 16 << 10
	// addAccountTimeout mencakup start runtime sementara untuk memeriksa token.
	addAccountTimeout = 2 * time.Minute
	// refreshTimeout mencakup start runtime beberapa akun sekaligus.
	refreshTimeout = 3 * time.Minute
)

type addAccountBody struct {
	Token string `json:"token"`
	Role  string `json:"role"`
	Label string `json:"label"`
	Actor string `json:"actor"`
}

type updateAccountBody struct {
	Role  *string `json:"role"`
	Label *string `json:"label"`
	Actor string  `json:"actor"`
}

type refreshBody struct {
	ID    string `json:"id"`
	Actor string `json:"actor"`
}

type strategyBody struct {
	Strategy string `json:"strategy"`
	Actor    string `json:"actor"`
}

// decodeLimited membaca body JSON berukuran terbatas. Pesan galat sengaja umum: isi body (bisa memuat token)
// tidak pernah dikutip balik.
func decodeLimited(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSONBody(w, r, dst, false)
}

// decodeJSONBody seperti decodeLimited; allowEmpty menerima body kosong (dst tetap nilai nol).
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAccountBody)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !(allowEmpty && errors.Is(err, io.EOF)) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "body permintaan tidak valid"})
		return false
	}
	return true
}

// writeAccountError memetakan galat pool ke status HTTP.
func writeAccountError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, runtime.ErrAccountInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, runtime.ErrAccountNotFound):
		status = http.StatusNotFound
	case errors.Is(err, runtime.ErrAccountDuplicate), errors.Is(err, runtime.ErrLastActive),
		errors.Is(err, runtime.ErrStoreUnavailable):
		status = http.StatusConflict
	case errors.Is(err, runtime.ErrAccountRejected):
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, errorBody{Error: err.Error()})
}

func validAccountID(id string) bool {
	if id == akun.LoginID {
		return true
	}
	if len(id) != 12 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// handleAccounts: GET /v1/accounts?days=30 — keadaan, pemakaian, dan perkiraan kuota setiap akun.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 90 {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "days harus 1..90"})
			return
		}
		days = n
	}
	writeJSON(w, http.StatusOK, s.svc.Overview(days))
}

// handleAddAccount: POST /v1/accounts — periksa token di runtime sementara lalu tambahkan ke pool.
func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	var body addAccountBody
	if !decodeLimited(w, r, &body) {
		return
	}
	ctx, cancel := contextWithTimeout(r, addAccountTimeout)
	defer cancel()
	st, err := s.svc.AddAccount(ctx, runtime.AddInput{
		Token: body.Token, Role: body.Role, Label: body.Label, Actor: body.Actor, Source: akun.SourceAdmin,
	})
	body.Token = ""
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"account": st})
}

// handleUpdateAccount: PATCH /v1/accounts/{id} — ubah peran dan/atau label.
func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validAccountID(id) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "id akun tidak valid"})
		return
	}
	var body updateAccountBody
	if !decodeLimited(w, r, &body) {
		return
	}
	if body.Role == nil && body.Label == nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "isi role dan/atau label"})
		return
	}
	st, err := s.svc.UpdateAccount(id, runtime.UpdateInput{Role: body.Role, Label: body.Label, Actor: body.Actor})
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": st})
}

// handleRemoveAccount: DELETE /v1/accounts/{id}?actor=… — keluarkan akun dari pool.
func (s *Server) handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validAccountID(id) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "id akun tidak valid"})
		return
	}
	if err := s.svc.RemoveAccount(id, r.URL.Query().Get("actor")); err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleRefreshAccounts: POST /v1/accounts/refresh — baca ulang identitas & kuota sekarang (id kosong = semua).
func (s *Server) handleRefreshAccounts(w http.ResponseWriter, r *http.Request) {
	var body refreshBody
	if !decodeJSONBody(w, r, &body, true) {
		return
	}
	if body.ID != "" && !validAccountID(body.ID) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "id akun tidak valid"})
		return
	}
	ctx, cancel := contextWithTimeout(r, refreshTimeout)
	defer cancel()
	if err := s.svc.RefreshAccounts(ctx, body.ID); err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.svc.Overview(30))
}

// handleSetStrategy: PUT /v1/accounts/strategy — ganti strategi pembagian permintaan.
func (s *Server) handleSetStrategy(w http.ResponseWriter, r *http.Request) {
	var body strategyBody
	if !decodeLimited(w, r, &body) {
		return
	}
	if err := s.svc.SetStrategy(strings.TrimSpace(body.Strategy), body.Actor); err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"strategy": s.svc.PoolStrategy()})
}
