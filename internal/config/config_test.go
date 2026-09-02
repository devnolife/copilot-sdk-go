package config

import (
	"reflect"
	"testing"
	"time"
)

// clearEnv mengosongkan semua variabel yang dibaca Load supaya nilai dari
// shell pengembang (mis. GITHUB_TOKEN) tidak bocor ke dalam test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"COPILOTD_ADDR", "COPILOTD_API_KEY",
		"TURNITIN_COPILOT_GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
		"TURNITIN_COPILOT_GITHUB_TOKENS", "COPILOT_GITHUB_TOKENS",
		"TURNITIN_COPILOT_MODEL", "COPILOT_MODEL",
		"TURNITIN_COPILOT_CHEAP_MODEL", "COPILOT_CHEAP_MODEL",
		"TURNITIN_COPILOT_HEAVY_MODEL", "COPILOT_HEAVY_MODEL",
		"TURNITIN_COPILOT_AGENT_MODEL", "COPILOT_AGENT_MODEL",
		"TURNITIN_COPILOT_MODEL_FALLBACKS", "COPILOT_MODEL_FALLBACKS",
		"TURNITIN_COPILOT_CLI_PATH", "COPILOT_CLI_PATH",
		"TURNITIN_COPILOT_CLI_URL", "COPILOT_CLI_URL",
		"TURNITIN_COPILOT_WORKING_DIR", "COPILOT_WORKING_DIR",
		"TURNITIN_COPILOT_BASE_DIR", "COPILOT_BASE_DIR",
		"TURNITIN_COPILOT_LOG_LEVEL",
		"TURNITIN_COPILOT_MAX_CONCURRENCY",
		"TURNITIN_COPILOT_TIMEOUT", "TURNITIN_COPILOT_AGENT_TIMEOUT",
		"TURNITIN_COPILOT_PROVIDER_TYPE", "COPILOT_PROVIDER_TYPE",
		"TURNITIN_COPILOT_PROVIDER_BASE_URL", "COPILOT_PROVIDER_BASE_URL",
		"TURNITIN_COPILOT_PROVIDER_API_KEY", "COPILOT_PROVIDER_API_KEY",
		"TURNITIN_COPILOT_PROVIDER_WIRE_API",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg := Load()

	if cfg.Addr != "127.0.0.1:8791" {
		t.Errorf("Addr = %q, ingin 127.0.0.1:8791", cfg.Addr)
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey = %q, ingin kosong", cfg.APIKey)
	}
	if cfg.MaxConcurrency != 2 {
		t.Errorf("MaxConcurrency = %d, ingin 2", cfg.MaxConcurrency)
	}
	if cfg.Timeout != 180*time.Second {
		t.Errorf("Timeout = %s, ingin 180s", cfg.Timeout)
	}
	if cfg.AgentTimeout != 300*time.Second {
		t.Errorf("AgentTimeout = %s, ingin 300s", cfg.AgentTimeout)
	}
	if cfg.LogLevel != "error" {
		t.Errorf("LogLevel = %q, ingin error", cfg.LogLevel)
	}
	if cfg.ProviderWireAPI != "completions" {
		t.Errorf("ProviderWireAPI = %q, ingin completions", cfg.ProviderWireAPI)
	}
	if cfg.GitHubTokens != nil || cfg.ModelFallbacks != nil {
		t.Errorf("daftar kosong harus nil, dapat tokens=%v fallbacks=%v", cfg.GitHubTokens, cfg.ModelFallbacks)
	}
	if got := cfg.Tokens(); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("Tokens() tanpa token = %v, ingin [\"\"] (pakai login copilot/gh)", got)
	}
}

func TestLoadPrefersTurnitinPrefixOverGeneric(t *testing.T) {
	clearEnv(t)
	t.Setenv("GITHUB_TOKEN", "generic")
	t.Setenv("COPILOT_GITHUB_TOKEN", "copilot")
	t.Setenv("TURNITIN_COPILOT_GITHUB_TOKEN", "turnitin")
	t.Setenv("COPILOT_MODEL", "gpt-generic")
	t.Setenv("TURNITIN_COPILOT_MODEL", "  claude-turnitin  ")

	cfg := Load()
	if cfg.GitHubToken != "turnitin" {
		t.Errorf("GitHubToken = %q, ingin prefix TURNITIN_ menang", cfg.GitHubToken)
	}
	if cfg.Model != "claude-turnitin" {
		t.Errorf("Model = %q, ingin nilai TURNITIN_ yang sudah di-trim", cfg.Model)
	}
}

func TestLoadFallsBackToGenericNames(t *testing.T) {
	clearEnv(t)
	t.Setenv("GH_TOKEN", "gh-only")
	t.Setenv("COPILOT_MODEL_FALLBACKS", "a, b ,,c")

	cfg := Load()
	if cfg.GitHubToken != "gh-only" {
		t.Errorf("GitHubToken = %q, ingin GH_TOKEN dipakai kalau yang lain kosong", cfg.GitHubToken)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(cfg.ModelFallbacks, want) {
		t.Errorf("ModelFallbacks = %v, ingin %v", cfg.ModelFallbacks, want)
	}
}

func TestLoadNumericEnv(t *testing.T) {
	cases := []struct {
		name        string
		concurrency string
		timeout     string
		wantConc    int
		wantTimeout time.Duration
	}{
		{"nilai valid", "5", "60", 5, 60 * time.Second},
		{"bukan angka pakai default", "banyak", "lama", 2, 180 * time.Second},
		{"nol dinaikkan ke satu", "0", "", 1, 180 * time.Second},
		{"negatif dinaikkan ke satu", "-3", "", 1, 180 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TURNITIN_COPILOT_MAX_CONCURRENCY", tc.concurrency)
			t.Setenv("TURNITIN_COPILOT_TIMEOUT", tc.timeout)
			cfg := Load()
			if cfg.MaxConcurrency != tc.wantConc {
				t.Errorf("MaxConcurrency = %d, ingin %d", cfg.MaxConcurrency, tc.wantConc)
			}
			if cfg.Timeout != tc.wantTimeout {
				t.Errorf("Timeout = %s, ingin %s", cfg.Timeout, tc.wantTimeout)
			}
		})
	}
}

func TestTokensPoolWinsOverSingleToken(t *testing.T) {
	cfg := Config{GitHubToken: "single", GitHubTokens: []string{"p1", "p2"}}
	if got := cfg.Tokens(); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Errorf("Tokens() = %v, ingin pool menang", got)
	}
	cfg = Config{GitHubToken: "single"}
	if got := cfg.Tokens(); !reflect.DeepEqual(got, []string{"single"}) {
		t.Errorf("Tokens() = %v, ingin [single]", got)
	}
}

func TestModelFor(t *testing.T) {
	cfg := Config{Model: "base", CheapModel: "cheap", AgentModel: "agent"}
	cases := map[string]string{
		"cheap": "cheap",
		"heavy": "base", // tidak dikonfigurasi → model utama
		"agent": "agent",
		"":      "base",
		"aneh":  "base",
	}
	for tier, want := range cases {
		if got := cfg.ModelFor(tier); got != want {
			t.Errorf("ModelFor(%q) = %q, ingin %q", tier, got, want)
		}
	}
}

func TestIsLocalProvider(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"", false}, // bukan BYOK
		{"http://localhost:11434/v1", true},
		{"http://127.0.0.1:11434", true},
		{"http://0.0.0.0:11434/v1", true},
		{"https://host.docker.internal:11434/v1", true},
		{"http://LOCALHOST/v1", true},
		{"http://[::1]:11434/v1", true},
		{"localhost:11434", true}, // tanpa skema
		{"https://api.openai.com/v1", false},
		{"http://10.0.0.5:11434/v1", false},
		{"http://localhost.evil.com/v1", false},
	}
	for _, tc := range cases {
		cfg := Config{ProviderBaseURL: tc.url}
		if got := cfg.IsLocalProvider(); got != tc.want {
			t.Errorf("IsLocalProvider(%q) = %v, ingin %v", tc.url, got, tc.want)
		}
		if got := cfg.IsBYOK(); got != (tc.url != "") {
			t.Errorf("IsBYOK(%q) = %v", tc.url, got)
		}
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(""); got != nil {
		t.Errorf("splitList(\"\") = %v, ingin nil", got)
	}
	if got := splitList(" , ,"); len(got) != 0 {
		t.Errorf("splitList hanya koma/spasi = %v, ingin kosong", got)
	}
	if got, want := splitList("x,  y ,z"), []string{"x", "y", "z"}; !reflect.DeepEqual(got, want) {
		t.Errorf("splitList = %v, ingin %v", got, want)
	}
}
