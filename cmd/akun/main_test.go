package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
)

func TestCommentOutTokenLinesDropsValues(t *testing.T) {
	in := "A=1\n# Pool akun Copilot\nTURNITIN_COPILOT_GITHUB_TOKENS=login,github_pat_rahasia\nTURNITIN_COPILOT_GITHUB_TOKEN=\n" +
		"export TURNITIN_COPILOT_GITHUB_TOKEN=\"gho_rahasia\"\n#TURNITIN_COPILOT_GITHUB_TOKENS=lama\nB=2\n"
	out, n := commentOutTokenLines(in, "dipindah ke akun.json")
	if n != 2 || strings.Contains(out, "rahasia") {
		t.Fatalf("n=%d out=%q", n, out)
	}
	for _, keep := range []string{"A=1\n", "B=2\n", "TURNITIN_COPILOT_GITHUB_TOKEN=\n", "#TURNITIN_COPILOT_GITHUB_TOKENS=lama\n",
		"# TURNITIN_COPILOT_GITHUB_TOKENS dipindah ke akun.json\n"} {
		if !strings.Contains(out, keep) {
			t.Errorf("baris %q hilang: %q", keep, out)
		}
	}
	if again, n2 := commentOutTokenLines(out, "x"); n2 != 0 || again != out {
		t.Fatal("migrasi kedua tidak boleh mengubah apa pun")
	}
}

func TestMigrasiMovesTokensAndBacksUpEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "TURNITIN_COPILOT_MODEL=gpt\nTURNITIN_COPILOT_GITHUB_TOKENS=login,github_pat_rahasia\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	cfg := config.Config{StateDir: state, GitHubTokens: []string{"login", "github_pat_rahasia"}, PoolStrategy: "proporsional"}
	backups := filepath.Join(dir, "cadangan")
	if err := migrasi(cfg, envPath, backups); err != nil {
		t.Fatal(err)
	}
	f, exists, err := akun.Load(akun.PathIn(state))
	if err != nil || !exists || len(f.Accounts) != 2 || f.Accounts[1].Token != "github_pat_rahasia" || f.Accounts[0].ID != akun.LoginID {
		t.Fatalf("akun.json = %+v (%v)", f, err)
	}
	raw, _ := os.ReadFile(envPath)
	if strings.Contains(string(raw), "rahasia") || !strings.Contains(string(raw), "TURNITIN_COPILOT_MODEL=gpt\n") {
		t.Fatalf(".env setelah migrasi = %q", raw)
	}
	if st, _ := os.Stat(envPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("izin .env = %v", st.Mode().Perm())
	}
	saved, _ := filepath.Glob(filepath.Join(backups, "copilot-go.env.*"))
	if len(saved) != 1 {
		t.Fatalf("cadangan .env = %v", saved)
	}
	if old, _ := os.ReadFile(saved[0]); string(old) != content {
		t.Fatal("cadangan harus berisi .env lama utuh")
	}
	// Dijalankan lagi: akun.json tetap, .env tidak diubah.
	if err := migrasi(cfg, envPath, backups); err != nil {
		t.Fatal(err)
	}
	if again, _ := filepath.Glob(filepath.Join(backups, "copilot-go.env.*")); len(again) != 1 {
		t.Fatal("migrasi kedua tidak boleh membuat cadangan baru")
	}
}

func TestDaemonCallReportsErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" { // seperti copilotd: health tanpa kunci
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.Header.Get("X-API-Key") != "kunci" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"akun sudah ada di pool"}`))
	}))
	defer srv.Close()
	d := daemon{base: srv.URL, key: "kunci"}
	if !d.alive() {
		t.Fatal("health harus hidup")
	}
	err := d.call(http.MethodPost, "/v1/accounts", map[string]string{"token": "x"}, time.Second, nil)
	if err == nil || err.Error() != "akun sudah ada di pool" {
		t.Fatalf("galat = %v", err)
	}
	if (daemon{base: "http://127.0.0.1:1"}).alive() {
		t.Fatal("alamat mati tidak boleh dianggap hidup")
	}
}

func TestLoadEnvFileDoesNotOverrideExistingEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("# komentar\nAKUN_UJI_A=\"dari-berkas\"\nexport AKUN_UJI_B=b\nAKUN_UJI_C=c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AKUN_UJI_C", "dari-shell")
	t.Setenv("AKUN_UJI_A", "")
	os.Unsetenv("AKUN_UJI_A")
	os.Unsetenv("AKUN_UJI_B")
	t.Cleanup(func() { os.Unsetenv("AKUN_UJI_B") })
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AKUN_UJI_A") != "dari-berkas" || os.Getenv("AKUN_UJI_B") != "b" || os.Getenv("AKUN_UJI_C") != "dari-shell" {
		t.Fatalf("A=%q B=%q C=%q", os.Getenv("AKUN_UJI_A"), os.Getenv("AKUN_UJI_B"), os.Getenv("AKUN_UJI_C"))
	}
}
