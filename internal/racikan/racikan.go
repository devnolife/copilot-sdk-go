// Package racikan menyimpan racikan model copilotd (racikan.json): model tiap tier yang diatur admin revisi.studio.
//
// Nilai di racikan.json menimpa setelan .env (TURNITIN_COPILOT_MODEL, _CHEAP_MODEL, _HEAVY_MODEL, _AGENT_MODEL,
// _MODEL_FALLBACKS) tanpa restart. Field yang tidak ada atau null mengikuti .env; string kosong berarti kosong
// (tier ikut model utama, model utama kosong = bawaan runtime).
package racikan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// FileName adalah nama berkas racikan di StateDir.
const FileName = "racikan.json"

// MaxFallbacks membatasi panjang rantai model cadangan.
const MaxFallbacks = 5

const (
	fileVersion = 1
	keepBackups = 20
)

var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$`)

// Models adalah model efektif tiap tier.
type Models struct {
	Model      string   `json:"model"`
	CheapModel string   `json:"cheap_model"`
	HeavyModel string   `json:"heavy_model"`
	AgentModel string   `json:"agent_model"`
	Fallbacks  []string `json:"fallbacks"`
}

// FromConfig mengambil model tiap tier dari setelan .env.
func FromConfig(cfg config.Config) Models {
	return Models{
		Model:      cfg.Model,
		CheapModel: cfg.CheapModel,
		HeavyModel: cfg.HeavyModel,
		AgentModel: cfg.AgentModel,
		Fallbacks:  append([]string{}, cfg.ModelFallbacks...),
	}
}

// For memilih model sesuai tier (sama dengan config.ModelFor): tier kosong ikut model utama.
func (m Models) For(tier string) string {
	switch tier {
	case "cheap":
		if m.CheapModel != "" {
			return m.CheapModel
		}
	case "heavy":
		if m.HeavyModel != "" {
			return m.HeavyModel
		}
	case "agent":
		if m.AgentModel != "" {
			return m.AgentModel
		}
	}
	return m.Model
}

// Override adalah isian admin. nil = ikut .env.
type Override struct {
	Model      *string   `json:"model,omitempty"`
	CheapModel *string   `json:"cheap_model,omitempty"`
	HeavyModel *string   `json:"heavy_model,omitempty"`
	AgentModel *string   `json:"agent_model,omitempty"`
	Fallbacks  *[]string `json:"fallbacks,omitempty"`
}

// Empty melaporkan apakah tidak ada satu pun field yang ditimpa.
func (o Override) Empty() bool {
	return o.Model == nil && o.CheapModel == nil && o.HeavyModel == nil && o.AgentModel == nil && o.Fallbacks == nil
}

// Apply menimpa base dengan field override yang terisi.
func (o Override) Apply(base Models) Models {
	out := base
	out.Fallbacks = append([]string{}, base.Fallbacks...)
	if o.Model != nil {
		out.Model = *o.Model
	}
	if o.CheapModel != nil {
		out.CheapModel = *o.CheapModel
	}
	if o.HeavyModel != nil {
		out.HeavyModel = *o.HeavyModel
	}
	if o.AgentModel != nil {
		out.AgentModel = *o.AgentModel
	}
	if o.Fallbacks != nil {
		out.Fallbacks = append([]string{}, (*o.Fallbacks)...)
	}
	return out
}

// Validate memeriksa bentuk nama model dan rantai cadangan (ketersediaan model diperiksa pemanggil).
func (o Override) Validate() error {
	for _, f := range []struct {
		name  string
		value *string
	}{{"tier utama", o.Model}, {"tier hemat", o.CheapModel}, {"tier berat (IDE Tugas)", o.HeavyModel}, {"agen", o.AgentModel}} {
		if f.value != nil && *f.value != "" && !modelID.MatchString(*f.value) {
			return fmt.Errorf("nama model %s tidak sah", f.name)
		}
	}
	if o.Fallbacks == nil {
		return nil
	}
	if len(*o.Fallbacks) > MaxFallbacks {
		return fmt.Errorf("model cadangan paling banyak %d", MaxFallbacks)
	}
	seen := map[string]bool{}
	for _, fb := range *o.Fallbacks {
		if !modelID.MatchString(fb) {
			return errors.New("nama model cadangan tidak sah")
		}
		if seen[fb] {
			return fmt.Errorf("model cadangan %s tercantum dua kali", fb)
		}
		seen[fb] = true
	}
	return nil
}

// File adalah isi racikan.json.
type File struct {
	Version   int        `json:"version"`
	Override  Override   `json:"override"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
}

// PathIn memberi lokasi racikan.json di stateDir ("" bila stateDir kosong).
func PathIn(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, FileName)
}

// Load membaca racikan.json. exists=false (tanpa galat) bila berkas belum ada. Isi yang tidak lolos Validate
// dianggap rusak supaya model asing tidak diam-diam dipakai.
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
		return File{}, true, fmt.Errorf("%s rusak: %w", path, err)
	}
	if err := f.Override.Validate(); err != nil {
		return File{}, true, fmt.Errorf("%s rusak: %w", path, err)
	}
	return f, true, nil
}

// Save menulis racikan.json secara atomik (0600, folder 0700); isi sebelumnya disimpan di <folder>/cadangan.
func Save(path string, f File) error {
	if err := f.Override.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("folder status: %w", err)
	}
	f.Version = fileVersion
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if prev, err := os.ReadFile(path); err == nil {
		if err := saveBackup(dir, prev); err != nil {
			return err
		}
	}
	return akun.WriteFileAtomic(path, append(raw, '\n'))
}

func saveBackup(dir string, data []byte) error {
	backups := filepath.Join(dir, "cadangan")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		return fmt.Errorf("folder cadangan: %w", err)
	}
	name := filepath.Join(backups, "racikan-"+time.Now().UTC().Format("20060102-150405.000000000")+".json")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return fmt.Errorf("menulis cadangan: %w", err)
	}
	entries, err := filepath.Glob(filepath.Join(backups, "racikan-*.json"))
	if err != nil || len(entries) <= keepBackups {
		return nil
	}
	sort.Strings(entries)
	for _, old := range entries[:len(entries)-keepBackups] {
		_ = os.Remove(old)
	}
	return nil
}
