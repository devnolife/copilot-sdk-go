// Package config membaca konfigurasi copilotd dari environment.
//
// Nama variabel sengaja dibuat sama dengan yang dipakai backend Python
// (prefix TURNITIN_COPILOT_*) supaya satu berkas .env bisa dipakai bersama.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config adalah seluruh setelan copilotd.
type Config struct {
	Addr   string
	APIKey string

	// Autentikasi
	GitHubToken  string
	GitHubTokens []string

	// Pool akun: akun dengan sisa kuota premium <= QuotaMinPercent hanya dipakai bila tidak ada akun lain
	// (0 = mati). Kuota tiap akun yang runtime-nya hidup dicek ulang tiap QuotaCheckInterval (0 = tidak dicek).
	QuotaMinPercent    float64
	QuotaCheckInterval time.Duration
	// RuntimeMaxAge: runtime yang hidup lebih lama dari ini diganti runtime baru (0 = tidak pernah). Runtime
	// membaca kuota dan daftar model akun saat menyala lalu menyimpannya selama hidup, jadi tanpa penggantian
	// data itu tidak pernah diperbarui.
	RuntimeMaxAge time.Duration

	// StateDir menyimpan akun.json (daftar akun, dikelola admin) dan pemakaian.json; kosong = tanpa berkas.
	StateDir string
	// PoolStrategy dipakai selama akun.json belum menyimpan strategi: proporsional | bergiliran | prioritas.
	PoolStrategy string
	// TimeZone menentukan batas hari pada catatan pemakaian harian.
	TimeZone string

	// Model
	Model          string
	CheapModel     string
	HeavyModel     string
	AgentModel     string
	ModelFallbacks []string

	// Runtime
	CLIPath        string
	CLIURL         string
	WorkingDir     string
	BaseDir        string
	LogLevel       string
	MaxConcurrency int

	// Timeout
	Timeout      time.Duration
	AgentTimeout time.Duration

	// BYOK
	ProviderType    string
	ProviderBaseURL string
	ProviderAPIKey  string
	ProviderWireAPI string
}

// Load membaca konfigurasi dari environment dengan default yang aman.
func Load() Config {
	cfg := Config{
		Addr:            envOr("COPILOTD_ADDR", "127.0.0.1:8791"),
		APIKey:          env("COPILOTD_API_KEY"),
		GitHubToken:     firstEnv("TURNITIN_COPILOT_GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"),
		Model:           firstEnv("TURNITIN_COPILOT_MODEL", "COPILOT_MODEL"),
		CheapModel:      firstEnv("TURNITIN_COPILOT_CHEAP_MODEL", "COPILOT_CHEAP_MODEL"),
		HeavyModel:      firstEnv("TURNITIN_COPILOT_HEAVY_MODEL", "COPILOT_HEAVY_MODEL"),
		AgentModel:      firstEnv("TURNITIN_COPILOT_AGENT_MODEL", "COPILOT_AGENT_MODEL"),
		CLIPath:         firstEnv("TURNITIN_COPILOT_CLI_PATH", "COPILOT_CLI_PATH"),
		CLIURL:          firstEnv("TURNITIN_COPILOT_CLI_URL", "COPILOT_CLI_URL"),
		WorkingDir:      firstEnv("TURNITIN_COPILOT_WORKING_DIR", "COPILOT_WORKING_DIR"),
		BaseDir:         firstEnv("TURNITIN_COPILOT_BASE_DIR", "COPILOT_BASE_DIR"),
		LogLevel:        envOr("TURNITIN_COPILOT_LOG_LEVEL", "error"),
		MaxConcurrency:  envInt("TURNITIN_COPILOT_MAX_CONCURRENCY", 2),
		Timeout:         time.Duration(envInt("TURNITIN_COPILOT_TIMEOUT", 180)) * time.Second,
		AgentTimeout:    time.Duration(envInt("TURNITIN_COPILOT_AGENT_TIMEOUT", 300)) * time.Second,
		ProviderType:    firstEnv("TURNITIN_COPILOT_PROVIDER_TYPE", "COPILOT_PROVIDER_TYPE"),
		ProviderBaseURL: firstEnv("TURNITIN_COPILOT_PROVIDER_BASE_URL", "COPILOT_PROVIDER_BASE_URL"),
		ProviderAPIKey:  firstEnv("TURNITIN_COPILOT_PROVIDER_API_KEY", "COPILOT_PROVIDER_API_KEY"),
		ProviderWireAPI: envOr("TURNITIN_COPILOT_PROVIDER_WIRE_API", "completions"),

		QuotaMinPercent:    envFloat("TURNITIN_COPILOT_QUOTA_MIN_PERCENT", 5),
		QuotaCheckInterval: time.Duration(envInt("TURNITIN_COPILOT_QUOTA_CHECK_SEC", 600)) * time.Second,
		RuntimeMaxAge:      time.Duration(envInt("TURNITIN_COPILOT_RUNTIME_MAX_AGE_SEC", 0)) * time.Second,

		StateDir:     envOr("TURNITIN_COPILOT_STATE_DIR", defaultStateDir()),
		PoolStrategy: strings.ToLower(envOr("TURNITIN_COPILOT_POOL_STRATEGY", "proporsional")),
		TimeZone:     envOr("TURNITIN_COPILOT_TIMEZONE", "Asia/Makassar"),
	}
	cfg.GitHubTokens = splitList(firstEnv("TURNITIN_COPILOT_GITHUB_TOKENS", "COPILOT_GITHUB_TOKENS"))
	cfg.ModelFallbacks = splitList(firstEnv("TURNITIN_COPILOT_MODEL_FALLBACKS", "COPILOT_MODEL_FALLBACKS"))
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	return cfg
}

// LoginEntry di TURNITIN_COPILOT_GITHUB_TOKENS berarti "akun login tersimpan" (copilot/gh) di posisi itu, sehingga
// akun yang sudah login tetap ikut pool bersama akun bertoken.
const LoginEntry = "login"

// Tokens mengembalikan daftar token untuk pool akun. Slice berisi satu string
// kosong berarti "pakai kredensial copilot/gh yang sudah login".
func (c Config) Tokens() []string {
	if len(c.GitHubTokens) > 0 {
		out := make([]string, 0, len(c.GitHubTokens))
		seen := map[string]bool{}
		for _, t := range c.GitHubTokens {
			if strings.EqualFold(t, LoginEntry) {
				t = ""
			}
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
		return out
	}
	if c.GitHubToken != "" {
		return []string{c.GitHubToken}
	}
	return []string{""}
}

// ModelFor memilih model sesuai tier yang diminta pemanggil.
func (c Config) ModelFor(tier string) string {
	switch tier {
	case "cheap":
		if c.CheapModel != "" {
			return c.CheapModel
		}
	case "heavy":
		if c.HeavyModel != "" {
			return c.HeavyModel
		}
	case "agent":
		if c.AgentModel != "" {
			return c.AgentModel
		}
	}
	return c.Model
}

// IsBYOK melaporkan apakah sesi memakai provider model sendiri.
func (c Config) IsBYOK() bool {
	return c.ProviderBaseURL != ""
}

// IsLocalProvider melaporkan apakah BYOK menunjuk endpoint di mesin ini.
// Dipakai backend untuk memutuskan boleh-tidaknya Copilot saat privacy mode.
func (c Config) IsLocalProvider() bool {
	if !c.IsBYOK() {
		return false
	}
	switch strings.ToLower(hostOf(c.ProviderBaseURL)) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "host.docker.internal":
		return true
	}
	return false
}

// hostOf mengambil nama host dari URL tanpa skema, port, dan path.
// Literal IPv6 berkurung ("[::1]:11434") dikembalikan tanpa kurungnya.
func hostOf(rawURL string) string {
	host := rawURL
	for _, prefix := range []string{"http://", "https://"} {
		host = strings.TrimPrefix(host, prefix)
	}
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end > 0 {
			return host[1:end]
		}
	}
	if idx := strings.IndexAny(host, ":/"); idx >= 0 {
		host = host[:idx]
	}
	return host
}

// defaultStateDir: $XDG_STATE_HOME/copilotd, atau ~/.local/state/copilotd.
func defaultStateDir() string {
	if dir := env("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "copilotd")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "copilotd")
}

func env(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func envOr(name, fallback string) string {
	if v := env(name); v != "" {
		return v
	}
	return fallback
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if v := env(name); v != "" {
			return v
		}
	}
	return ""
}

func envInt(name string, fallback int) int {
	v := env(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envFloat(name string, fallback float64) float64 {
	v := env(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(strings.Replace(v, ",", ".", 1), 64)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func splitList(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
