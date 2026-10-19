package api

import (
	"net/http"
	"testing"

	"github.com/devnolife/copilot-sdk-go/internal/config"
	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

func racikanHandler(t *testing.T, stateDir string) http.Handler {
	t.Helper()
	cfg := config.Config{StateDir: stateDir, MaxConcurrency: 1, Model: "gpt-6.1-sol", HeavyModel: "claude-opus-5",
		ModelFallbacks: []string{"claude-sonnet-5"}}
	return New(runtime.NewService(cfg), "kunci").Handler()
}

func TestRacikanEndpointsRequireKey(t *testing.T) {
	h := racikanHandler(t, t.TempDir())
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if rec := do(t, h, method, "/v1/config/models", `{"override":{}}`, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s tanpa kunci = %d, ingin 401", method, rec.Code)
		}
	}
}

func TestRacikanPutGetAndReset(t *testing.T) {
	h := racikanHandler(t, t.TempDir())
	rec := do(t, h, http.MethodPut, "/v1/config/models",
		`{"override":{"heavy_model":"gpt-6-luna","agent_model":"","fallbacks":["gpt-6-luna","claude-haiku-4.5"]},"actor":"admin:a@b"}`, keyHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	eff := body["effective"].(map[string]any)
	env := body["env"].(map[string]any)
	if eff["heavy_model"] != "gpt-6-luna" || env["heavy_model"] != "claude-opus-5" || body["updated_by"] != "admin:a@b" {
		t.Errorf("respons PUT salah: %v", body)
	}
	ov := body["override"].(map[string]any)
	if v, ok := ov["agent_model"]; !ok || v != "" {
		t.Errorf("agent_model kosong harus tersimpan sebagai \"\": %v", ov)
	}
	if _, ok := ov["model"]; ok {
		t.Errorf("model tidak ditimpa, tidak boleh muncul di override: %v", ov)
	}

	status := decodeBody(t, do(t, h, http.MethodGet, "/v1/status", "", keyHeader))
	if status["agent_model"] != "gpt-6.1-sol" {
		t.Errorf("status agent_model = %v, ingin ikut model utama", status["agent_model"])
	}

	// null = kembali ke .env.
	rec = do(t, h, http.MethodPut, "/v1/config/models", `{"override":{"heavy_model":null},"actor":"admin:a@b"}`, keyHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT reset = %d %s", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, do(t, h, http.MethodGet, "/v1/config/models", "", keyHeader))
	if got["effective"].(map[string]any)["heavy_model"] != "claude-opus-5" || got["stored"] != true {
		t.Errorf("setelah reset harus ikut .env: %v", got)
	}
}

func TestRacikanPutRejectsBadInput(t *testing.T) {
	h := racikanHandler(t, t.TempDir())
	for _, body := range []string{
		`{bukan json`,
		`{"actor":"admin"}`,
		`{"override":{"model":"gpt 6"}}`,
		`{"override":{"fallbacks":["a","a"]}}`,
		`{"override":{"fallbacks":["a","b","c","d","e","f"]}}`,
	} {
		if rec := do(t, h, http.MethodPut, "/v1/config/models", body, keyHeader); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, ingin 400", body, rec.Code)
		}
	}
	readOnly := racikanHandler(t, "")
	if rec := do(t, readOnly, http.MethodPut, "/v1/config/models", `{"override":{"model":"gpt-6-luna"}}`, keyHeader); rec.Code != http.StatusConflict {
		t.Errorf("tanpa StateDir = %d, ingin 409", rec.Code)
	}
}
