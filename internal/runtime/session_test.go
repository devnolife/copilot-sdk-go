package runtime

import (
	"reflect"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

func newTestService(cfg config.Config) *Service {
	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = 1
	}
	return NewService(cfg)
}

func TestResolveModel(t *testing.T) {
	svc := newTestService(config.Config{Model: "base", CheapModel: "cheap", AgentModel: "agent"})
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{"eksplisit menang atas tier", Request{Model: "explicit", Tier: "cheap"}, "explicit"},
		{"tier cheap", Request{Tier: "cheap"}, "cheap"},
		{"tier agent", Request{Tier: "agent"}, "agent"},
		{"tier tak dikenal ke default", Request{Tier: "heavy"}, "base"},
		{"tanpa apa pun ke default", Request{}, "base"},
	}
	for _, tc := range cases {
		if got := svc.resolveModel(tc.req); got != tc.want {
			t.Errorf("%s: resolveModel = %q, ingin %q", tc.name, got, tc.want)
		}
	}
}

func TestModelChain(t *testing.T) {
	svc := newTestService(config.Config{
		Model:          "primary",
		CheapModel:     "cheap",
		ModelFallbacks: []string{"fb1", "primary", "fb2"},
	})

	if got, want := svc.modelChain(Request{}), []string{"primary", "fb1", "fb2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("modelChain default = %v, ingin %v (primary tidak diulang)", got, want)
	}
	if got, want := svc.modelChain(Request{Tier: "cheap"}), []string{"cheap", "fb1", "primary", "fb2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("modelChain tier = %v, ingin %v", got, want)
	}
	if got, want := svc.modelChain(Request{Model: "forced"}), []string{"forced"}; !reflect.DeepEqual(got, want) {
		t.Errorf("modelChain model eksplisit = %v, ingin %v (tanpa fallback)", got, want)
	}

	noFallback := newTestService(config.Config{Model: "only"})
	if got, want := noFallback.modelChain(Request{}), []string{"only"}; !reflect.DeepEqual(got, want) {
		t.Errorf("modelChain tanpa fallback = %v, ingin %v", got, want)
	}
}

func TestTimeoutPrefersRequestValue(t *testing.T) {
	svc := newTestService(config.Config{})
	if got := svc.timeout(Request{Timeout: 7 * time.Second}, time.Minute); got != 7*time.Second {
		t.Errorf("timeout = %s, ingin 7s dari request", got)
	}
	if got := svc.timeout(Request{}, time.Minute); got != time.Minute {
		t.Errorf("timeout = %s, ingin fallback 1m", got)
	}
	if got := svc.timeout(Request{Timeout: -1}, time.Minute); got != time.Minute {
		t.Errorf("timeout negatif harus ke fallback, dapat %s", got)
	}
}

func decisionType(t *testing.T, fn func(copilot.PermissionRequest, copilot.PermissionInvocation) (rpc.PermissionDecision, error)) rpc.PermissionDecision {
	t.Helper()
	if fn == nil {
		t.Fatal("OnPermissionRequest nil")
	}
	// PermissionRequest adalah interface; handler kita mengabaikan isinya.
	d, err := fn(nil, copilot.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSessionConfigSandboxByDefault(t *testing.T) {
	svc := newTestService(config.Config{})
	cfg := svc.sessionConfig(Request{}, "m", nil)

	if cfg.Model != "m" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.Streaming == nil || !*cfg.Streaming {
		t.Error("Streaming harus true")
	}
	if !reflect.DeepEqual(cfg.AvailableTools, []string{noTools}) {
		t.Errorf("AvailableTools = %v, ingin hanya %q (semua tool bawaan terkunci)", cfg.AvailableTools, noTools)
	}
	if _, ok := decisionType(t, cfg.OnPermissionRequest).(*rpc.PermissionDecisionReject); !ok {
		t.Error("mode default harus menolak semua permintaan izin")
	}
	if cfg.SystemMessage != nil {
		t.Errorf("SystemMessage harus nil tanpa system/json_mode, dapat %+v", cfg.SystemMessage)
	}
	if cfg.Provider != nil {
		t.Error("Provider harus nil tanpa BYOK")
	}
}

func TestSessionConfigWebMode(t *testing.T) {
	svc := newTestService(config.Config{})
	cfg := svc.sessionConfig(Request{Web: true}, "m", nil)

	if !reflect.DeepEqual(cfg.AvailableTools, webTools) {
		t.Errorf("AvailableTools = %v, ingin %v", cfg.AvailableTools, webTools)
	}
	if _, ok := decisionType(t, cfg.OnPermissionRequest).(*rpc.PermissionDecisionApproveOnce); !ok {
		t.Error("mode web harus menyetujui izin web_search/web_fetch")
	}
}

func TestSessionConfigCallerTools(t *testing.T) {
	svc := newTestService(config.Config{})
	tools := []copilot.Tool{{Name: "cari_paper"}, {Name: "baca_pdf"}}
	cfg := svc.sessionConfig(Request{Web: true}, "m", tools)

	if len(cfg.Tools) != 2 {
		t.Fatalf("Tools = %d, ingin 2", len(cfg.Tools))
	}
	if !reflect.DeepEqual(cfg.AvailableTools, []string{"cari_paper", "baca_pdf"}) {
		t.Errorf("AvailableTools = %v, ingin hanya nama tool pemanggil (web diabaikan)", cfg.AvailableTools)
	}
	if _, ok := decisionType(t, cfg.OnPermissionRequest).(*rpc.PermissionDecisionApproveOnce); !ok {
		t.Error("tool milik pemanggil harus disetujui otomatis")
	}
}

func TestSessionConfigSystemAndJSONMode(t *testing.T) {
	svc := newTestService(config.Config{})

	cfg := svc.sessionConfig(Request{System: "Kamu asisten."}, "m", nil)
	if cfg.SystemMessage == nil || cfg.SystemMessage.Content != "Kamu asisten." || cfg.SystemMessage.Mode != "replace" {
		t.Errorf("SystemMessage = %+v", cfg.SystemMessage)
	}

	cfg = svc.sessionConfig(Request{JSONMode: true}, "m", nil)
	if cfg.SystemMessage == nil || cfg.SystemMessage.Content != jsonInstruction {
		t.Errorf("json_mode tanpa system: SystemMessage = %+v", cfg.SystemMessage)
	}

	cfg = svc.sessionConfig(Request{System: "Kamu asisten.", JSONMode: true}, "m", nil)
	want := "Kamu asisten.\n\n" + jsonInstruction
	if cfg.SystemMessage == nil || cfg.SystemMessage.Content != want {
		t.Errorf("json_mode + system: Content = %q, ingin %q", cfg.SystemMessage.Content, want)
	}
}

func TestSessionConfigBYOKProvider(t *testing.T) {
	svc := newTestService(config.Config{
		ProviderType:    "openai",
		ProviderBaseURL: "http://localhost:11434/v1",
		ProviderAPIKey:  "k",
		ProviderWireAPI: "completions",
	})
	cfg := svc.sessionConfig(Request{}, "qwen", nil)
	if cfg.Provider == nil {
		t.Fatal("Provider harus diisi saat BYOK")
	}
	want := &copilot.ProviderConfig{Type: "openai", BaseURL: "http://localhost:11434/v1", APIKey: "k", WireAPI: "completions"}
	if !reflect.DeepEqual(cfg.Provider, want) {
		t.Errorf("Provider = %+v, ingin %+v", cfg.Provider, want)
	}
}

func TestModelLabel(t *testing.T) {
	if got := modelLabel(""); got != "default" {
		t.Errorf("modelLabel(\"\") = %q", got)
	}
	if got := modelLabel("gpt"); got != "gpt" {
		t.Errorf("modelLabel(gpt) = %q", got)
	}
}

func TestServiceExposesConfig(t *testing.T) {
	cfg := config.Config{Model: "x", MaxConcurrency: 3}
	svc := NewService(cfg)
	if !reflect.DeepEqual(svc.Config(), cfg) {
		t.Errorf("Config() = %+v, ingin %+v", svc.Config(), cfg)
	}
	svc.Shutdown() // pool dingin — tidak boleh panic
}
