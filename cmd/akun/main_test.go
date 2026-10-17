package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddTokenStartsPoolWithStoredLogin(t *testing.T) {
	in := "TURNITIN_COPILOT_MODEL=gpt-6.1-sol\nTURNITIN_COPILOT_MAX_CONCURRENCY=2\n"
	out, n := addToken(in, "github_pat_baru", "")
	if n != 2 || !strings.Contains(out, "TURNITIN_COPILOT_GITHUB_TOKENS=login,github_pat_baru\n") {
		t.Fatalf("n=%d out=%q", n, out)
	}
	if !strings.HasPrefix(out, in) {
		t.Fatalf("baris lain harus utuh: %q", out)
	}
}

func TestAddTokenAppendsToExistingPoolAndReplacesEmptyLine(t *testing.T) {
	out, n := addToken("A=1\nTURNITIN_COPILOT_GITHUB_TOKENS=login,gho_lama\nB=2\n", "github_pat_baru", "")
	if n != 3 || !strings.Contains(out, "\nTURNITIN_COPILOT_GITHUB_TOKENS=login,gho_lama,github_pat_baru\nB=2\n") {
		t.Fatalf("n=%d out=%q", n, out)
	}
	out, n = addToken("TURNITIN_COPILOT_GITHUB_TOKENS=\nB=2\n", "gho_baru", "github_pat_tunggal")
	if n != 2 || !strings.HasPrefix(out, "TURNITIN_COPILOT_GITHUB_TOKENS=github_pat_tunggal,gho_baru\nB=2") {
		t.Fatalf("token tunggal yang sedang dipakai harus jadi akun #1: n=%d out=%q", n, out)
	}
}

func TestWriteAtomicKeepsSecretFilePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte("A=2\n")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	raw, _ := os.ReadFile(path)
	if st.Mode().Perm() != 0o600 || string(raw) != "A=2\n" {
		t.Fatalf("mode=%v isi=%q", st.Mode().Perm(), raw)
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
