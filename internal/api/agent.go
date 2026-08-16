package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

// agentRequest menjalankan sesi agentik: model boleh memanggil tool yang
// dideklarasikan pemanggil.
//
// Eksekusi tool tetap terjadi di sisi pemanggil (backend Python) karena di
// sanalah database, model ML, dan konteks pengguna berada. copilotd memanggil
// balik lewat CallbackURL setiap kali model butuh sebuah tool.
type agentRequest struct {
	Prompt        string             `json:"prompt"`
	System        string             `json:"system"`
	Model         string             `json:"model"`
	Tier          string             `json:"tier"`
	TimeoutSec    float64            `json:"timeout_sec"`
	Tools         []runtime.ToolSpec `json:"tools"`
	CallbackURL   string             `json:"callback_url"`
	CallbackToken string             `json:"callback_token"`
	// CallbackHeaders diteruskan apa adanya ke setiap panggilan balik,
	// misalnya X-API-Key milik backend pemanggil.
	CallbackHeaders map[string]string `json:"callback_headers"`
}

// toolCallbackRequest adalah payload yang dikirim ke pemanggil.
type toolCallbackRequest struct {
	Token     string         `json:"token"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// toolCallbackResponse adalah balasan yang diharapkan dari pemanggil.
type toolCallbackResponse struct {
	Result string `json:"result"`
	Error  string `json:"error"`
}

// toolCallbackTimeout membatasi berapa lama menunggu pemanggil mengeksekusi
// satu tool. Tool berat (RAG, model ML) bisa lambat, tapi tidak boleh
// menggantung sesi selamanya.
const toolCallbackTimeout = 120 * time.Second

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	var body agentRequest
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "JSON tidak valid"})
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "prompt kosong"})
		return
	}
	if len(body.Tools) > 0 && body.CallbackURL == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "callback_url wajib saat mengirim tools"})
		return
	}

	req := runtime.Request{
		Prompt:  strings.TrimSpace(body.Prompt),
		System:  body.System,
		Model:   body.Model,
		Tier:    firstNonEmpty(body.Tier, "agent"),
		Timeout: time.Duration(body.TimeoutSec * float64(time.Second)),
	}

	emit, flush, ok := startNDJSON(w)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, 0)
	defer cancel()

	exec := s.toolExecutor(body.CallbackURL, body.CallbackToken, body.CallbackHeaders)
	if err := s.svc.Stream(ctx, req, body.Tools, exec, emit); err != nil {
		emit(runtime.Event{Type: "error", Message: err.Error()})
	}
	flush()
}

// toolExecutor membuat callback HTTP ke pemanggil untuk satu sesi agent.
func (s *Server) toolExecutor(callbackURL, token string, headers map[string]string) runtime.ToolExecutor {
	if callbackURL == "" {
		return nil
	}
	client := &http.Client{Timeout: toolCallbackTimeout}

	return func(ctx context.Context, name string, args map[string]any) (string, error) {
		payload, err := json.Marshal(toolCallbackRequest{
			Token:     token,
			Tool:      name,
			Arguments: args,
		})
		if err != nil {
			return "", fmt.Errorf("gagal menyusun payload tool: %w", err)
		}

		reqCtx, cancel := context.WithTimeout(ctx, toolCallbackTimeout)
		defer cancel()

		httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, callbackURL, bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			httpReq.Header.Set(key, value)
		}

		resp, err := client.Do(httpReq)
		if err != nil {
			return "", fmt.Errorf("callback tool gagal: %w", err)
		}
		defer resp.Body.Close()

		var out toolCallbackResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("%w: balasan tidak bisa dibaca", errNoToolResult)
		}
		if resp.StatusCode >= 400 {
			if out.Error != "" {
				return "", fmt.Errorf("callback tool ditolak: %s", out.Error)
			}
			return "", fmt.Errorf("callback tool ditolak (%d)", resp.StatusCode)
		}
		if out.Error != "" {
			return "", fmt.Errorf("%s", out.Error)
		}
		return out.Result, nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
