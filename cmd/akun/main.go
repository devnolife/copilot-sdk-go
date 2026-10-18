// Command akun mengelola pool akun Copilot milik copilotd tanpa pernah mencetak token.
//
//	go run ./cmd/akun                                      # daftar akun: peran, login, status, kuota, reset
//	AKUN_TOKEN_BARU=… go run ./cmd/akun cek                # periksa token tanpa menambahkannya
//	AKUN_TOKEN_BARU=… go run ./cmd/akun tambah [--peran utama|cadangan] [--label teks]
//	go run ./cmd/akun migrasi [--env .env] [--cadangan DIR] # pindahkan token .env ke akun.json
//
// Bila copilotd sedang jalan, `daftar` dan `tambah` memakai API-nya: akun baru langsung ikut pembagian tanpa
// restart. Bila tidak, `tambah` menulis akun.json dan berlaku saat copilotd dinyalakan. Akun juga bisa dikelola
// dari halaman admin revisi.studio (Akun Copilot).
//
// Token akun baru hanya dibaca dari variabel lingkungan AKUN_TOKEN_BARU (bukan argumen, yang terlihat di daftar
// proses). scripts/tambah-akun.sh memintanya secara tersembunyi.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

const tokenEnv = "AKUN_TOKEN_BARU"

// tokenKeys: variabel .env lama yang dipindahkan oleh `migrasi`.
var tokenKeys = []string{"TURNITIN_COPILOT_GITHUB_TOKENS", "TURNITIN_COPILOT_GITHUB_TOKEN"}

func main() {
	args := os.Args[1:]
	cmd := "daftar"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("akun "+cmd, flag.ExitOnError)
	envPath := fs.String("env", ".env", "berkas .env copilotd (dibaca untuk konfigurasi; diubah oleh 'migrasi')")
	home, _ := os.UserHomeDir()
	backupDir := fs.String("cadangan", filepath.Join(home, "backups", "copilotd-env"), "folder cadangan .env sebelum diubah")
	role := fs.String("peran", akun.RoleUtama, "peran akun baru: utama | cadangan")
	label := fs.String("label", "", "label akun baru (opsional, maks 60 karakter)")
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
		err = tambah(cfg, newToken(), *role, *label)
	case "migrasi":
		err = migrasi(cfg, *envPath, *backupDir)
	default:
		err = fmt.Errorf("perintah tidak dikenal %q (daftar | cek | tambah | migrasi)", cmd)
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

func actor() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, _ := os.Hostname()
	return "cli:" + name + "@" + host
}

// daemon adalah klien API copilotd di mesin ini.
type daemon struct {
	base string
	key  string
}

func newDaemon(cfg config.Config) daemon {
	return daemon{base: "http://" + cfg.Addr, key: cfg.APIKey}
}

func (d daemon) alive() bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(d.base + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// call mengirim permintaan ke copilotd; body boleh nil. Galat memuat pesan dari copilotd (tanpa token).
func (d daemon) call(method, path string, body any, timeout time.Duration, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.key != "" {
		req.Header.Set("X-API-Key", d.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("copilotd tidak menjawab: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

var wita = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Makassar"); err == nil {
		return loc
	}
	return time.FixedZone("WITA", 8*3600)
}()

func formatReset(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.In(wita).Format("2 Jan 15.04") + " WITA"
}

func quotaCell(q *runtime.QuotaInfo, quotaErr string) string {
	switch {
	case q == nil && quotaErr != "":
		return "tidak terbaca"
	case q == nil:
		return "belum dibaca"
	case q.Unlimited:
		return "tak terbatas"
	}
	return fmt.Sprintf("%.1f%% (%d/%d terpakai)", q.RemainingPercent, q.Used, q.Entitlement)
}

func daftar(cfg config.Config) error {
	d := newDaemon(cfg)
	if !d.alive() {
		fmt.Printf("copilotd tidak menjawab di %s; memeriksa akun langsung (menyalakan runtime sementara per akun).\n\n", d.base)
		return daftarOffline(cfg)
	}
	var ov runtime.Overview
	if err := d.call(http.MethodGet, "/v1/accounts?days=7", nil, 20*time.Second, &ov); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "#\tid\tperan\takun\tjenis\tstatus\tsisa kuota\treset\t7 hari (porsi)\n")
	for _, a := range ov.Accounts {
		name := a.Login
		if name == "" {
			name = "-"
		}
		if a.Label != "" {
			name += " · " + a.Label
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d (%.0f%%)\n", a.Index, a.ID, a.Role, name, a.Kind, a.State,
			quotaCell(a.Quota, a.QuotaError), formatReset(a.ResetAt), a.Usage.Last7d.Requests, a.Usage.Share7dPercent)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nStrategi: %s · sumber daftar: %s · ambang hampir habis: %.1f%% · %d permintaan hari ini\n",
		ov.Strategy, ov.Source, ov.QuotaMinPercent, ov.Totals.RequestsToday)
	if ov.Source != akun.FileName {
		fmt.Println("Daftar masih dari .env: jalankan `go run ./cmd/akun migrasi` agar akun bisa dikelola dari admin.")
	}
	return nil
}

// accountRecords: daftar akun menurut akun.json bila ada, selain itu .env.
func accountRecords(cfg config.Config) (akun.File, bool, error) {
	path := akun.PathIn(cfg.StateDir)
	if path != "" {
		f, exists, err := akun.Load(path)
		if err != nil || exists {
			return f, exists, err
		}
	}
	return akun.FromTokens(cfg.Tokens(), cfg.PoolStrategy, time.Now()), false, nil
}

func daftarOffline(cfg config.Config) error {
	f, _, err := accountRecords(cfg)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "#\tid\tperan\tjenis\takun\tsisa kuota\treset\n")
	for i, r := range f.Accounts {
		p, err := probe(cfg, r.Token)
		if err != nil {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t-\tGAGAL\t%v\n", i+1, r.ID, r.Role, runtime.TokenKind(r.Token), err)
			continue
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", i+1, r.ID, r.Role, p.Kind, p.Login,
			quotaCell(p.Quota, p.QuotaError), formatReset(p.ResetAt))
	}
	return w.Flush()
}

func probe(cfg config.Config, token string) (runtime.AccountProbe, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return runtime.ProbeAccount(ctx, cfg, token)
}

func cek(cfg config.Config, token string) (runtime.AccountProbe, error) {
	p, err := probe(cfg, token)
	if err != nil {
		return p, err
	}
	plan := p.Plan
	if plan == "" {
		plan = "-"
	}
	fmt.Printf("Token sah: akun %s (%s, plan %s), sisa kuota %s, reset %s\n", p.Login, p.Kind, plan,
		quotaCell(p.Quota, p.QuotaError), formatReset(p.ResetAt))
	if p.Quota != nil && p.Quota.OverageAllowed {
		fmt.Println("Catatan: setelah kuota habis akun ini tetap jalan sebagai kelebihan pemakaian berbayar.")
	}
	if p.Quota == nil {
		fmt.Println("Peringatan: kuota akun tidak terbaca; pastikan akun ini punya langganan Copilot.")
	}
	return p, nil
}

func tambah(cfg config.Config, token, role, label string) error {
	d := newDaemon(cfg)
	if d.alive() {
		var res struct {
			Account runtime.AccountStatus `json:"account"`
		}
		body := map[string]string{"token": token, "role": role, "label": label, "actor": actor()}
		if err := d.call(http.MethodPost, "/v1/accounts", body, 150*time.Second, &res); err != nil {
			return err
		}
		a := res.Account
		fmt.Printf("Akun %s ditambahkan sebagai #%d (%s, id %s), sisa kuota %s, reset %s.\n", a.Login, a.Index, a.Role,
			a.ID, quotaCell(a.Quota, a.QuotaError), formatReset(a.ResetAt))
		fmt.Println("Langsung ikut pembagian permintaan, tanpa restart. Periksa: go run ./cmd/akun")
		return nil
	}
	return tambahOffline(cfg, token, role, label)
}

// tambahOffline menulis akun baru ke akun.json saat copilotd tidak jalan.
func tambahOffline(cfg config.Config, token, role, label string) error {
	path := akun.PathIn(cfg.StateDir)
	if path == "" {
		return fmt.Errorf("TURNITIN_COPILOT_STATE_DIR kosong; tidak ada tempat menyimpan akun")
	}
	if !akun.ValidRole(role) || role == akun.RoleNonaktif {
		return fmt.Errorf("peran %q tidak bisa dipakai untuk akun baru (utama | cadangan)", role)
	}
	if strings.HasPrefix(token, "ghp_") {
		return fmt.Errorf("PAT klasik (ghp_…) tidak didukung; pakai github_pat_… dengan izin Copilot Requests")
	}
	f, _, err := accountRecords(cfg)
	if err != nil {
		return err
	}
	id := akun.Fingerprint(token)
	for _, r := range f.Accounts {
		if r.ID == id {
			return fmt.Errorf("token ini sudah ada di pool (id %s)", id)
		}
	}
	p, err := cek(cfg, token)
	if err != nil {
		return err
	}
	for _, r := range f.Accounts {
		if p.Login != "" && strings.EqualFold(r.Login, p.Login) {
			return fmt.Errorf("akun %s sudah ada di pool (id %s); token kedua untuk akun yang sama tidak menambah kuota", p.Login, r.ID)
		}
	}
	now := time.Now().UTC()
	f.Accounts = append(f.Accounts, akun.Record{
		ID: id, Token: token, Source: akun.SourceCLI, Role: role, Label: strings.TrimSpace(label), AddedAt: now,
		AddedBy: actor(), Identity: akun.Identity{Login: p.Login, Plan: p.Plan, ResetAt: p.ResetAt,
			TokenBasedBilling: p.TokenBasedBilling, CheckedAt: &now},
	})
	if err := akun.Save(path, f, true); err != nil {
		return err
	}
	fmt.Printf("Akun %s ditulis ke %s (copilotd tidak jalan). Berlaku saat copilotd dinyalakan:\n", p.Login, path)
	fmt.Println("  systemctl --user restart revisi-copilotd")
	return nil
}

// migrasi memindahkan TURNITIN_COPILOT_GITHUB_TOKEN(S) dari .env ke akun.json, lalu mengganti baris .env itu dengan
// komentar (nilai lama hanya tersisa di cadangan .env).
func migrasi(cfg config.Config, envPath, backupDir string) error {
	path := akun.PathIn(cfg.StateDir)
	if path == "" {
		return fmt.Errorf("TURNITIN_COPILOT_STATE_DIR kosong; tidak ada tempat menyimpan akun")
	}
	f, exists, err := akun.Load(path)
	if err != nil {
		return err
	}
	envTokens := akun.FromTokens(cfg.Tokens(), cfg.PoolStrategy, time.Now())
	if !exists {
		if err := akun.Save(path, envTokens, false); err != nil {
			return err
		}
		fmt.Printf("%s dibuat dengan %d akun dari .env (strategi %s).\n", path, len(envTokens.Accounts), envTokens.Strategy)
	} else {
		known := map[string]bool{}
		for _, r := range f.Accounts {
			known[r.ID] = true
		}
		added := 0
		for _, r := range envTokens.Accounts {
			if !known[r.ID] {
				f.Accounts = append(f.Accounts, r)
				added++
			}
		}
		if added > 0 {
			if err := akun.Save(path, f, true); err != nil {
				return err
			}
		}
		fmt.Printf("%s sudah ada (%d akun); %d akun dari .env ditambahkan.\n", path, len(f.Accounts), added)
	}

	raw, err := os.ReadFile(envPath)
	if err != nil {
		return fmt.Errorf("membaca %s: %w", envPath, err)
	}
	note := fmt.Sprintf("dipindah ke %s oleh cmd/akun migrasi, %s", path, time.Now().Format("2006-01-02"))
	updated, n := commentOutTokenLines(string(raw), note)
	if n == 0 {
		fmt.Printf("%s tidak lagi berisi token pool; tidak ada yang diubah.\n", envPath)
	} else {
		if err := backup(envPath, raw, backupDir); err != nil {
			return err
		}
		if err := akun.WriteFileAtomic(envPath, []byte(updated)); err != nil {
			return err
		}
		fmt.Printf("%d baris token di %s diganti komentar.\n", n, envPath)
	}
	fmt.Println("Aktifkan dengan: systemctl --user restart revisi-copilotd")
	return nil
}

// commentOutTokenLines mengganti baris TURNITIN_COPILOT_GITHUB_TOKEN(S)=<nilai> yang aktif dengan komentar tanpa
// nilai. Baris kosong-nilai dibiarkan.
func commentOutTokenLines(content, note string) (string, int) {
	lines := strings.Split(content, "\n")
	n := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		for _, key := range tokenKeys {
			value, ok := strings.CutPrefix(trimmed, key+"=")
			if !ok || strings.Trim(strings.TrimSpace(value), `"'`) == "" {
				continue
			}
			lines[i] = "# " + key + " " + note
			n++
		}
	}
	return strings.Join(lines, "\n"), n
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
