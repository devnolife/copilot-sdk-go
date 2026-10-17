// Command akun mengelola pool akun Copilot milik copilotd tanpa pernah mencetak token.
//
//	go run ./cmd/akun                      # daftar akun pool: login, status, sisa kuota
//	AKUN_TOKEN_BARU=… go run ./cmd/akun cek
//	AKUN_TOKEN_BARU=… go run ./cmd/akun tambah [--env .env] [--cadangan DIR]
//
// Token akun baru hanya dibaca dari variabel lingkungan AKUN_TOKEN_BARU (bukan argumen, yang terlihat di daftar
// proses). scripts/tambah-akun.sh memintanya secara tersembunyi. Setelah menambah akun, copilotd perlu di-restart.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/config"
	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

const (
	tokenEnv  = "AKUN_TOKEN_BARU"
	tokensKey = "TURNITIN_COPILOT_GITHUB_TOKENS"
)

func main() {
	args := os.Args[1:]
	cmd := "daftar"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("akun "+cmd, flag.ExitOnError)
	envPath := fs.String("env", ".env", "berkas .env copilotd (dibaca untuk konfigurasi; ditulis oleh 'tambah')")
	home, _ := os.UserHomeDir()
	backupDir := fs.String("cadangan", filepath.Join(home, "backups", "copilotd-env"), "folder cadangan .env sebelum diubah")
	_ = fs.Parse(args)

	if err := loadEnvFile(*envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fail("membaca %s: %v", *envPath, err)
	}
	cfg := config.Load()
	var err error
	switch cmd {
	case "daftar":
		err = daftar(cfg)
	case "cek":
		_, err = cek(cfg, newToken())
	case "tambah":
		err = tambah(cfg, newToken(), *envPath, *backupDir)
	default:
		err = fmt.Errorf("perintah tidak dikenal %q (daftar | cek | tambah)", cmd)
	}
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "akun: "+format+"\n", args...)
	os.Exit(1)
}

func newToken() string {
	token := strings.TrimSpace(os.Getenv(tokenEnv))
	if token == "" {
		fail("%s kosong; jalankan scripts/tambah-akun.sh agar token diminta tanpa tampil di layar", tokenEnv)
	}
	return token
}

func probe(cfg config.Config, token string) (runtime.AccountProbe, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return runtime.ProbeAccount(ctx, cfg, token)
}

func quotaText(p runtime.AccountProbe) (string, string) {
	q := p.Quota
	switch {
	case q == nil && p.QuotaError != "":
		return "tidak terbaca", p.QuotaError
	case q == nil:
		return "-", ""
	case q.Unlimited:
		return "tak terbatas", q.Type
	}
	note := q.Type
	if q.OverageAllowed {
		note += "; setelah habis jalan sebagai kelebihan pemakaian berbayar"
	}
	return fmt.Sprintf("%.1f%% (%d/%d terpakai)", q.RemainingPercent, q.Used, q.Entitlement), note
}

func daftar(cfg config.Config) error {
	tokens := cfg.Tokens()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "#\tjenis\takun\tsisa kuota\tcatatan\n")
	for i, token := range tokens {
		p, err := probe(cfg, token)
		if err != nil {
			fmt.Fprintf(w, "%d\t%s\t-\tGAGAL\t%v\n", i+1, runtime.TokenKind(token), err)
			continue
		}
		sisa, note := quotaText(p)
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", i+1, p.Kind, p.Login, sisa, note)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nAmbang hampir habis: %.1f%% (TURNITIN_COPILOT_QUOTA_MIN_PERCENT); urutan = prioritas pemakaian.\n",
		cfg.QuotaMinPercent)
	return nil
}

func cek(cfg config.Config, token string) (runtime.AccountProbe, error) {
	p, err := probe(cfg, token)
	if err != nil {
		return p, err
	}
	sisa, note := quotaText(p)
	fmt.Printf("Token sah: akun %s (%s, autentikasi %s), sisa kuota %s %s\n", p.Login, p.Kind, p.AuthType, sisa, note)
	if p.Quota == nil {
		fmt.Println("Peringatan: kuota akun tidak terbaca; pastikan akun ini punya langganan Copilot.")
	}
	return p, nil
}

func tambah(cfg config.Config, token, envPath, backupDir string) error {
	for _, existing := range cfg.Tokens() {
		if existing == token {
			return fmt.Errorf("token ini sudah ada di pool")
		}
	}
	p, err := cek(cfg, token)
	if err != nil {
		return err
	}
	for i, existing := range cfg.Tokens() {
		other, err := probe(cfg, existing)
		if err == nil && other.Login != "" && strings.EqualFold(other.Login, p.Login) {
			return fmt.Errorf("akun %s sudah ada di pool (#%d); token kedua untuk akun yang sama tidak menambah kuota", p.Login, i+1)
		}
	}
	raw, err := os.ReadFile(envPath)
	if err != nil {
		return fmt.Errorf("membaca %s: %w", envPath, err)
	}
	if err := backup(envPath, raw, backupDir); err != nil {
		return err
	}
	updated, count := addToken(string(raw), token, cfg.GitHubToken)
	if err := writeAtomic(envPath, []byte(updated)); err != nil {
		return err
	}
	fmt.Printf("Akun %s ditambahkan sebagai #%d. Aktifkan dengan: systemctl --user restart revisi-copilotd\n", p.Login, count)
	fmt.Println("Periksa sesudahnya: go run ./cmd/akun")
	return nil
}

// addToken menambahkan token ke TURNITIN_COPILOT_GITHUB_TOKENS. Bila pool belum diisi, akun yang dipakai sekarang
// ditaruh lebih dulu: "login" (login tersimpan) atau token tunggal TURNITIN_COPILOT_GITHUB_TOKEN. Mengembalikan isi
// baru dan jumlah akun.
func addToken(content, token, single string) (string, int) {
	lines := strings.Split(content, "\n")
	idx := -1
	var current []string
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), tokensKey+"=") {
			idx = i
			value := strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
			current = nil
			for _, part := range strings.Split(strings.Trim(value, `"'`), ",") {
				if part = strings.TrimSpace(part); part != "" {
					current = append(current, part)
				}
			}
		}
	}
	if len(current) == 0 {
		base := config.LoginEntry
		if single != "" {
			base = single
		}
		current = []string{base}
	}
	current = append(current, token)
	line := tokensKey + "=" + strings.Join(current, ",")
	if idx >= 0 {
		lines[idx] = line
	} else {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "# Pool akun Copilot (urutan = prioritas; \"login\" = akun login tersimpan). Kelola: scripts/tambah-akun.sh", line, "")
	}
	return strings.Join(lines, "\n"), len(current)
}

func backup(envPath string, raw []byte, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("folder cadangan: %w", err)
	}
	name := filepath.Join(dir, "copilot-go.env."+time.Now().Format("20060102-150405"))
	if err := os.WriteFile(name, raw, 0o600); err != nil {
		return fmt.Errorf("menulis cadangan: %w", err)
	}
	fmt.Printf("Cadangan %s: %s\n", filepath.Base(envPath), name)
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// loadEnvFile mengisi variabel lingkungan dari berkas KEY=VALUE yang belum diset (format EnvironmentFile systemd).
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, set := os.LookupEnv(key); !set {
			_ = os.Setenv(key, value)
		}
	}
	return sc.Err()
}
