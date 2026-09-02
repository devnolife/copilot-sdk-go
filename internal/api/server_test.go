package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/config"
	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

// newTestHandler membangun server dengan pool dingin: runtime Copilot tidak
// pernah dinyalakan selama test hanya menyentuh jalur validasi.
func newTestHandler(cfg config.Config, apiKey string) http.Handler {
	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = 1
	}
	return New(runtime.NewService(cfg), apiKey).Handler()
}

func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body bukan JSON: %v — %q", err, rec.Body.String())
	}
	return out
}

func TestHealthNeedsNoAuth(t *testing.T) {
	h := newTestHandler(config.Config{}, "rahasia")
	rec := do(t, h, http.MethodGet, "/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := decodeBody(t, rec); body["ok"] != true {
		t.Errorf("body = %v", body)
	}
}

func TestAuthMiddleware(t *testing.T) {
	h := newTestHandler(config.Config{}, "rahasia")

	if rec := do(t, h, http.MethodGet, "/v1/status", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("tanpa header: status = %d, ingin 401", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/v1/status", "", map[string]string{"X-API-Key": "salah"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("key salah: status = %d, ingin 401", rec.Code)
	} else if body := decodeBody(t, rec); body["error"] != "API key salah" {
		t.Errorf("pesan error = %v", body["error"])
	}
	if rec := do(t, h, http.MethodGet, "/v1/status", "", map[string]string{"X-API-Key": "rahasia"}); rec.Code != http.StatusOK {
		t.Errorf("key benar: status = %d, ingin 200", rec.Code)
	}

	// Tanpa API key terkonfigurasi, semua endpoint terbuka.
	open := newTestHandler(config.Config{}, "")
	if rec := do(t, open, http.MethodGet, "/v1/status", "", nil); rec.Code != http.StatusOK {
		t.Errorf("auth mati: status = %d, ingin 200", rec.Code)
	}
}

func TestStatusReportsConfigWithoutStartingRuntime(t *testing.T) {
	h := newTestHandler(config.Config{GitHubTokens: []string{"a", "b"}, MaxConcurrency: 3}, "")
	body := decodeBody(t, do(t, h, http.MethodGet, "/v1/status", "", nil))

	if body["accounts"] != float64(2) {
		t.Errorf("accounts = %v, ingin 2", body["accounts"])
	}
	if body["model"] != "(runtime default)" || body["agent_model"] != "(runtime default)" {
		t.Errorf("model kosong harus dilaporkan sebagai runtime default: %v", body)
	}
	if body["auth"] != "github" || body["local_only"] != false || body["max_concurrency"] != float64(3) {
		t.Errorf("body = %v", body)
	}

	byok := newTestHandler(config.Config{
		Model:           "qwen",
		AgentModel:      "qwen-agent",
		ProviderBaseURL: "http://localhost:11434/v1",
	}, "")
	body = decodeBody(t, do(t, byok, http.MethodGet, "/v1/status", "", nil))
	if body["auth"] != "byok" || body["local_only"] != true || body["provider_base_url"] != "http://localhost:11434/v1" {
		t.Errorf("BYOK lokal: body = %v", body)
	}
	if body["model"] != "qwen" || body["agent_model"] != "qwen-agent" {
		t.Errorf("model per tier tidak dilaporkan: %v", body)
	}
}

func TestGenerateAndStreamRejectBadInput(t *testing.T) {
	h := newTestHandler(config.Config{}, "")
	for _, path := range []string{"/v1/generate", "/v1/stream", "/stream"} {
		rec := do(t, h, http.MethodPost, path, "{bukan json", nil)
		if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "JSON tidak valid" {
			t.Errorf("%s JSON rusak: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		rec = do(t, h, http.MethodPost, path, `{"prompt":"   "}`, nil)
		if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "prompt kosong" {
			t.Errorf("%s prompt spasi: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if rec := do(t, h, http.MethodGet, path, "", nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET: status=%d, ingin 405", path, rec.Code)
		}
	}
}

func TestAgentValidation(t *testing.T) {
	h := newTestHandler(config.Config{}, "")

	rec := do(t, h, http.MethodPost, "/v1/agent", `{"prompt":""}`, nil)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "prompt kosong" {
		t.Errorf("prompt kosong: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodPost, "/v1/agent", `{"prompt":"halo","tools":[{"name":"x"}]}`, nil)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "callback_url wajib saat mengirim tools" {
		t.Errorf("tools tanpa callback: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	h := newTestHandler(config.Config{}, "")
	if rec := do(t, h, http.MethodGet, "/v1/tidak-ada", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, ingin 404", rec.Code)
	}
}

func TestGenerateRequestToRuntime(t *testing.T) {
	req := generateRequest{Prompt: "  halo  ", System: "s", Model: "m", Tier: "cheap", JSONMode: true, Web: true, TimeoutSec: 1.5}
	got := req.toRuntime()
	if got.Prompt != "halo" || got.System != "s" || got.Model != "m" || got.Tier != "cheap" || !got.JSONMode || !got.Web {
		t.Errorf("toRuntime = %+v", got)
	}
	if got.Timeout != 1500*time.Millisecond {
		t.Errorf("Timeout = %s, ingin 1.5s", got.Timeout)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "agent"); got != "agent" {
		t.Errorf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty("", " "); got != "" {
		t.Errorf("firstNonEmpty semua kosong = %q", got)
	}
}

func TestStartNDJSONHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	emit, flush, ok := startNDJSON(rec)
	if !ok {
		t.Fatal("ResponseRecorder mendukung Flusher, harus ok")
	}
	emit(runtime.Event{Type: "delta", Content: "ha"})
	emit(runtime.Event{Type: "done", Content: "halo", Model: "copilot:x"})
	flush()

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("header anti-buffering hilang: %v", rec.Header())
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("ingin 2 baris NDJSON, dapat %d: %q", len(lines), rec.Body.String())
	}
	var last runtime.Event
	if err := json.Unmarshal([]byte(lines[1]), &last); err != nil || last.Type != "done" || last.Content != "halo" {
		t.Errorf("baris terakhir = %q (%v)", lines[1], err)
	}
}

// noFlushWriter adalah ResponseWriter tanpa Flusher.
type noFlushWriter struct{ http.ResponseWriter }

func TestStartNDJSONWithoutFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, ok := startNDJSON(noFlushWriter{rec})
	if ok {
		t.Fatal("tanpa Flusher harus gagal")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, ingin 500", rec.Code)
	}
}

// ── callback tool ke pemanggil ─────────────────────────────────────────

type callbackCapture struct {
	headers http.Header
	body    toolCallbackRequest
}

func callbackServer(t *testing.T, status int, response string, capture *callbackCapture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			capture.headers = r.Header.Clone()
			_ = json.NewDecoder(r.Body).Decode(&capture.body)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
}

func TestToolExecutorNilWithoutCallbackURL(t *testing.T) {
	s := New(runtime.NewService(config.Config{MaxConcurrency: 1}), "")
	if s.toolExecutor("", "tok", nil) != nil {
		t.Fatal("tanpa callback_url executor harus nil")
	}
}

func TestToolExecutorForwardsPayloadAndHeaders(t *testing.T) {
	var got callbackCapture
	srv := callbackServer(t, http.StatusOK, `{"result":"5 paper"}`, &got)
	defer srv.Close()

	s := New(runtime.NewService(config.Config{MaxConcurrency: 1}), "")
	exec := s.toolExecutor(srv.URL, "tok-123", map[string]string{"X-API-Key": "kunci-backend"})

	result, err := exec(context.Background(), "cari_paper", map[string]any{"q": "fuzzy", "n": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if result != "5 paper" {
		t.Errorf("result = %q", result)
	}
	if got.headers.Get("Content-Type") != "application/json" || got.headers.Get("X-API-Key") != "kunci-backend" {
		t.Errorf("header callback = %v", got.headers)
	}
	if got.body.Token != "tok-123" || got.body.Tool != "cari_paper" {
		t.Errorf("payload = %+v", got.body)
	}
	if got.body.Arguments["q"] != "fuzzy" || got.body.Arguments["n"] != float64(5) {
		t.Errorf("arguments = %v", got.body.Arguments)
	}
}

func TestToolExecutorErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		response   string
		wantSubstr string
		wantNoRes  bool
	}{
		{"error di body 200", http.StatusOK, `{"error":"tabel kosong"}`, "tabel kosong", false},
		{"4xx dengan pesan", http.StatusForbidden, `{"error":"token salah"}`, "ditolak: token salah", false},
		{"5xx tanpa pesan", http.StatusInternalServerError, `{}`, "ditolak (500)", false},
		{"balasan bukan JSON", http.StatusOK, `<html>maintenance</html>`, "balasan tidak bisa dibaca", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := callbackServer(t, tc.status, tc.response, nil)
			defer srv.Close()

			s := New(runtime.NewService(config.Config{MaxConcurrency: 1}), "")
			exec := s.toolExecutor(srv.URL, "", nil)
			_, err := exec(context.Background(), "x", nil)
			if err == nil {
				t.Fatal("ingin error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, ingin mengandung %q", err, tc.wantSubstr)
			}
			if errors.Is(err, errNoToolResult) != tc.wantNoRes {
				t.Errorf("errors.Is(errNoToolResult) = %v, ingin %v", !tc.wantNoRes, tc.wantNoRes)
			}
		})
	}
}

func TestToolExecutorUnreachableCallback(t *testing.T) {
	srv := callbackServer(t, http.StatusOK, `{}`, nil)
	srv.Close() // langsung dimatikan → koneksi ditolak

	s := New(runtime.NewService(config.Config{MaxConcurrency: 1}), "")
	exec := s.toolExecutor(srv.URL, "", nil)
	_, err := exec(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "callback tool gagal") {
		t.Errorf("error = %v, ingin 'callback tool gagal'", err)
	}
}
