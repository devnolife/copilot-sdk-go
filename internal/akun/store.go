// Package akun menyimpan daftar akun Copilot milik copilotd (akun.json) tanpa pernah mencetak token.
//
// akun.json menjadi sumber daftar akun begitu ada: dibuat oleh `cmd/akun migrasi` atau otomatis pada perubahan
// pertama lewat admin. Selama belum ada, copilotd memakai TURNITIN_COPILOT_GITHUB_TOKEN(S) dari .env.
package akun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// Peran akun di pool.
const (
	RoleUtama    = "utama"    // ikut pembagian permintaan
	RoleCadangan = "cadangan" // dipakai bila akun utama penuh atau tidak tersedia
	RoleNonaktif = "nonaktif" // tidak pernah dipakai
)

// Strategi pembagian permintaan di antara akun yang setara.
const (
	StrategyProporsional = "proporsional" // bobot = sisa unit kuota
	StrategyBergiliran   = "bergiliran"   // bobot sama
	StrategyPrioritas    = "prioritas"    // urutan daftar
)

// Asal akun.
const (
	SourceLogin = "login" // akun login tersimpan copilot/gh
	SourceEnv   = "env"   // dari .env
	SourceAdmin = "admin" // ditambahkan lewat admin revisi.studio
	SourceCLI   = "cli"   // ditambahkan lewat cmd/akun
)

// LoginID adalah id akun login tersimpan (tanpa token).
const LoginID = "login"

// FileName adalah nama berkas daftar akun di StateDir.
const FileName = "akun.json"

const (
	fileVersion = 1
	keepBackups = 20
)

// Roles dan Strategies: nilai yang sah, sesuai urutan tampilan.
var (
	Roles      = []string{RoleUtama, RoleCadangan, RoleNonaktif}
	Strategies = []string{StrategyProporsional, StrategyBergiliran, StrategyPrioritas}
)

// ValidRole melaporkan apakah v peran yang dikenal.
func ValidRole(v string) bool { return slices.Contains(Roles, v) }

// ValidStrategy melaporkan apakah v strategi yang dikenal.
func ValidStrategy(v string) bool { return slices.Contains(Strategies, v) }

// Identity adalah data akun dari Copilot (bukan rahasia). Disimpan supaya tetap tampil walau runtime akun belum
// hidup; tanggal reset hanya berubah sebulan sekali.
type Identity struct {
	Login             string     `json:"login,omitempty"`
	Plan              string     `json:"plan,omitempty"`
	ResetAt           *time.Time `json:"reset_at,omitempty"`
	TokenBasedBilling *bool      `json:"token_based_billing,omitempty"`
	CheckedAt         *time.Time `json:"identity_checked_at,omitempty"`
}

// Record adalah satu akun di akun.json. Token kosong = akun login tersimpan.
type Record struct {
	ID      string    `json:"id"`
	Token   string    `json:"token,omitempty"`
	Source  string    `json:"source"`
	Role    string    `json:"role"`
	Label   string    `json:"label,omitempty"`
	AddedAt time.Time `json:"added_at"`
	AddedBy string    `json:"added_by,omitempty"`
	Identity
}

// File adalah isi akun.json.
type File struct {
	Version  int      `json:"version"`
	Strategy string   `json:"strategy"`
	Accounts []Record `json:"accounts"`
}

// Fingerprint memberi id akun yang aman ditampilkan: 12 hex pertama SHA-256 token, atau LoginID.
func Fingerprint(token string) string {
	if token == "" {
		return LoginID
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

// PathIn memberi lokasi akun.json di stateDir ("" bila stateDir kosong).
func PathIn(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, FileName)
}

// FromTokens menyusun daftar akun dari token .env (string kosong / "login" = akun login tersimpan).
func FromTokens(tokens []string, strategy string, now time.Time) File {
	f := File{Version: fileVersion, Strategy: strategy}
	seen := map[string]bool{}
	for _, token := range tokens {
		if strings.EqualFold(token, config.LoginEntry) {
			token = ""
		}
		id := Fingerprint(token)
		if seen[id] {
			continue
		}
		seen[id] = true
		source := SourceEnv
		if token == "" {
			source = SourceLogin
		}
		f.Accounts = append(f.Accounts, Record{ID: id, Token: token, Source: source, Role: RoleUtama, AddedAt: now.UTC()})
	}
	f.normalize()
	return f
}

// normalize memperbaiki isi yang tidak konsisten: id dihitung ulang dari token, peran/strategi asing jadi bawaan,
// akun ganda dibuang.
func (f *File) normalize() {
	if !ValidStrategy(f.Strategy) {
		f.Strategy = StrategyProporsional
	}
	seen := map[string]bool{}
	out := make([]Record, 0, len(f.Accounts))
	for _, r := range f.Accounts {
		r.ID = Fingerprint(r.Token)
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		if !ValidRole(r.Role) {
			r.Role = RoleUtama
		}
		if r.Source == "" {
			r.Source = SourceEnv
			if r.Token == "" {
				r.Source = SourceLogin
			}
		}
		out = append(out, r)
	}
	f.Accounts = out
}

// Load membaca akun.json. exists=false (tanpa galat) bila berkas belum ada.
func Load(path string) (File, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, fmt.Errorf("membaca %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		// Pesan galat json hanya memuat posisi/tipe, bukan isi berkas, jadi token tidak ikut tercetak.
		return File{}, true, fmt.Errorf("%s rusak: %w", path, err)
	}
	f.normalize()
	return f, true, nil
}

// Save menulis akun.json secara atomik (berkas 0600, folder 0700). backup=true menyimpan isi sebelumnya ke
// <folder>/cadangan (20 terbaru); dipakai untuk perubahan oleh admin/CLI, bukan pembaruan cache identitas.
func Save(path string, f File, backup bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("folder status: %w", err)
	}
	// Folder status khusus copilotd: rapatkan juga bila sudah ada dengan izin lebih longgar.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("folder status: %w", err)
	}
	f.Version = fileVersion
	f.normalize()
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if backup {
		if prev, err := os.ReadFile(path); err == nil {
			if err := saveBackup(dir, prev); err != nil {
				return err
			}
		}
	}
	return WriteFileAtomic(path, append(raw, '\n'))
}

func saveBackup(dir string, data []byte) error {
	backups := filepath.Join(dir, "cadangan")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		return fmt.Errorf("folder cadangan: %w", err)
	}
	name := filepath.Join(backups, "akun-"+time.Now().UTC().Format("20060102-150405.000000000")+".json")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return fmt.Errorf("menulis cadangan: %w", err)
	}
	entries, err := filepath.Glob(filepath.Join(backups, "akun-*.json"))
	if err != nil || len(entries) <= keepBackups {
		return nil
	}
	sort.Strings(entries)
	for _, old := range entries[:len(entries)-keepBackups] {
		_ = os.Remove(old)
	}
	return nil
}

// WriteFileAtomic menulis data lewat berkas sementara di folder yang sama lalu rename, dengan izin 0600.
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
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
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
