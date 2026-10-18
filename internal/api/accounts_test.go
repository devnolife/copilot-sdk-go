package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

const testToken = "github_pat_11UJI_rahasia_sekali"

// accountsHandler: pool dengan StateDir sementara dan probe palsu (tidak ada runtime Copilot yang dinyalakan).
func accountsHandler(t *testing.T, probeErr error) http.Handler {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir(), GitHubTokens: []string{"login"}, MaxConcurrency: 1,
		QuotaMinPercent: 5, QuotaCheckInterval: time.Hour, TimeZone: "Asia/Makassar"}
	svc := runtime.NewServiceWithOptions(cfg, runtime.Options{
		Probe: func(_ context.Context, _ config.Config, token string) (runtime.AccountProbe, error) {
			if probeErr != nil {
				return runtime.AccountProbe{}, probeErr
			}
			reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			login := "mutia"
			if strings.HasPrefix(token, "gho_") {
				login = "akun-lain"
			}
			return runtime.AccountProbe{Kind: runtime.TokenKind(token), Authenticated: true, Login: login,
				Plan: "enterprise", ResetAt: &reset,
				Quota: &runtime.QuotaInfo{Type: "premium_interactions", RemainingPercent: 100, Entitlement: 500000}}, nil
		},
	})
	return New(svc, "kunci").Handler()
}

var keyHeader = map[string]string{"X-API-Key": "kunci", "Content-Type": "application/json"}

func noToken(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, "rahasia") || strings.Contains(body, "bocor") {
		t.Fatalf("respons memuat token: %s", body)
	}
}

func TestAccountsEndpointsRequireKey(t *testing.T) {
	h := accountsHandler(t, nil)
	for _, rt := range [][2]string{{http.MethodGet, "/v1/accounts"}, {http.MethodPost, "/v1/accounts"},
		{http.MethodPost, "/v1/accounts/refresh"}, {http.MethodPut, "/v1/accounts/strategy"},
		{http.MethodPatch, "/v1/accounts/login"}, {http.MethodDelete, "/v1/accounts/login"}} {
		if rec := do(t, h, rt[0], rt[1], "{}", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s tanpa kunci = %d, ingin 401", rt[0], rt[1], rec.Code)
		}
	}
}

func TestAccountsOverviewAndAddFlow(t *testing.T) {
	h := accountsHandler(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/accounts?days=7", "", keyHeader)
	body := decodeBody(t, rec)
	if rec.Code != http.StatusOK || body["strategy"] != "proporsional" || body["source"] != ".env" || body["days"] != float64(7) {
		t.Fatalf("overview awal = %d %v", rec.Code, body)
	}
	if accs := body["accounts"].([]any); len(accs) != 1 || accs[0].(map[string]any)["id"] != akun.LoginID {
		t.Fatalf("akun awal = %v", body["accounts"])
	}
	for _, q := range []string{"days=0", "days=91", "days=x"} {
		if rec := do(t, h, http.MethodGet, "/v1/accounts?"+q, "", keyHeader); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, ingin 400", q, rec.Code)
		}
	}

	rec = do(t, h, http.MethodPost, "/v1/accounts", `{"token":"`+testToken+`","role":"cadangan","label":"Lab","actor":"admin:a@b.c"}`, keyHeader)
	noToken(t, rec.Body.String())
	if rec.Code != http.StatusCreated {
		t.Fatalf("tambah = %d %s", rec.Code, rec.Body.String())
	}
	account := decodeBody(t, rec)["account"].(map[string]any)
	if account["login"] != "mutia" || account["role"] != "cadangan" || account["label"] != "Lab" ||
		account["id"] != akun.Fingerprint(testToken) || account["kind"] != "pat-berbutir" || account["source"] != "admin" {
		t.Fatalf("akun baru = %v", account)
	}

	rec = do(t, h, http.MethodGet, "/v1/accounts", "", keyHeader)
	noToken(t, rec.Body.String())
	body = decodeBody(t, rec)
	if body["source"] != "akun.json" || len(body["accounts"].([]any)) != 2 {
		t.Fatalf("setelah tambah: %v", body)
	}
	totals := body["totals"].(map[string]any)
	if totals["accounts"] != float64(2) || totals["reserve"] != float64(1) || totals["quota_entitlement"] != float64(500000) {
		t.Fatalf("totals = %v", totals)
	}
}

func TestAddAccountErrorsMapToStatus(t *testing.T) {
	h := accountsHandler(t, nil)
	cases := []struct {
		body string
		want int
	}{
		{`{"token":"ghp_klasik_rahasia"}`, http.StatusBadRequest},
		{`{"token":""}`, http.StatusBadRequest},
		{`{"token":"` + testToken + `","extra":1}`, http.StatusBadRequest},
		{`{"token":` + strings.Repeat(`"x`, 9000) + `"}`, http.StatusBadRequest},
		{`bukan json rahasia`, http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := do(t, h, http.MethodPost, "/v1/accounts", c.body, keyHeader)
		noToken(t, rec.Body.String())
		if rec.Code != c.want {
			t.Errorf("body %.40q = %d, ingin %d (%s)", c.body, rec.Code, c.want, rec.Body.String())
		}
	}
	if rec := do(t, h, http.MethodPost, "/v1/accounts", `{"token":"`+testToken+`"}`, keyHeader); rec.Code != http.StatusCreated {
		t.Fatalf("tambah: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/v1/accounts", `{"token":"`+testToken+`"}`, keyHeader); rec.Code != http.StatusConflict {
		t.Fatalf("token ganda = %d, ingin 409", rec.Code)
	}

	rejected := accountsHandler(t, errors.New("Not authenticated: gho_bocor ditolak"))
	rec := do(t, rejected, http.MethodPost, "/v1/accounts", `{"token":"gho_bocor"}`, keyHeader)
	noToken(t, rec.Body.String())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("token ditolak = %d, ingin 422", rec.Code)
	}
}

func TestUpdateRemoveStrategyEndpoints(t *testing.T) {
	h := accountsHandler(t, nil)
	if rec := do(t, h, http.MethodPost, "/v1/accounts", `{"token":"`+testToken+`"}`, keyHeader); rec.Code != http.StatusCreated {
		t.Fatalf("tambah: %d", rec.Code)
	}
	id := akun.Fingerprint(testToken)
	checks := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPatch, "/v1/accounts/" + id, `{"role":"nonaktif","actor":"admin:x"}`, http.StatusOK},
		{http.MethodPatch, "/v1/accounts/login", `{"role":"nonaktif"}`, http.StatusConflict},
		{http.MethodPatch, "/v1/accounts/login", `{}`, http.StatusBadRequest},
		{http.MethodPatch, "/v1/accounts/BUKAN-ID", `{"role":"utama"}`, http.StatusBadRequest},
		{http.MethodPatch, "/v1/accounts/aaaaaaaaaaaa", `{"role":"utama"}`, http.StatusNotFound},
		{http.MethodPatch, "/v1/accounts/" + id, `{"role":"raja"}`, http.StatusBadRequest},
		{http.MethodPut, "/v1/accounts/strategy", `{"strategy":"acak"}`, http.StatusBadRequest},
		{http.MethodPut, "/v1/accounts/strategy", `{"strategy":"bergiliran","actor":"admin:x"}`, http.StatusOK},
		{http.MethodPost, "/v1/accounts/refresh", `{"id":"aaaaaaaaaaaa"}`, http.StatusNotFound},
		{http.MethodPost, "/v1/accounts/refresh", `{"id":"../x"}`, http.StatusBadRequest},
		{http.MethodDelete, "/v1/accounts/login?actor=admin:x", "", http.StatusConflict},
		{http.MethodDelete, "/v1/accounts/" + id + "?actor=admin:x", "", http.StatusOK},
		{http.MethodDelete, "/v1/accounts/" + id, "", http.StatusNotFound},
	}
	for _, c := range checks {
		rec := do(t, h, c.method, c.path, c.body, keyHeader)
		if rec.Code != c.want {
			t.Errorf("%s %s %s = %d, ingin %d (%s)", c.method, c.path, c.body, rec.Code, c.want, rec.Body.String())
		}
	}
	body := decodeBody(t, do(t, h, http.MethodGet, "/v1/accounts", "", keyHeader))
	if body["strategy"] != "bergiliran" || len(body["accounts"].([]any)) != 1 {
		t.Fatalf("akhir: %v", body)
	}
	status := decodeBody(t, do(t, h, http.MethodGet, "/v1/status", "", keyHeader))
	if status["pool_strategy"] != "bergiliran" || status["accounts"] != float64(1) {
		t.Fatalf("status = %v", status)
	}
}
