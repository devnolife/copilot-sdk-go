package runtime

import (
	"errors"
	"log"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/racikan"
)

// RacikanState adalah racikan model untuk admin: nilai efektif, bawaan .env, isian admin, dan asal berkasnya.
type RacikanState struct {
	Effective racikan.Models   `json:"effective"`
	Env       racikan.Models   `json:"env"`
	Override  racikan.Override `json:"override"`
	UpdatedAt *time.Time       `json:"updated_at,omitempty"`
	UpdatedBy string           `json:"updated_by,omitempty"`
	// Stored: racikan.json ada dan terbaca. Error: berkas ada tetapi rusak (racikan .env yang dipakai).
	Stored bool   `json:"stored"`
	Error  string `json:"error,omitempty"`
	// Persistent: StateDir terisi, jadi perubahan admin bertahan setelah restart.
	Persistent bool `json:"persistent"`
}

// ErrRacikanReadOnly: StateDir kosong, racikan tidak bisa disimpan.
var ErrRacikanReadOnly = errors.New("TURNITIN_COPILOT_STATE_DIR kosong: racikan model tidak bisa disimpan")

// loadRacikan membaca racikan.json saat service dibuat. Berkas rusak dicatat dan .env tetap dipakai.
func (s *Service) loadRacikan() {
	path := racikan.PathIn(s.cfg.StateDir)
	if path == "" {
		return
	}
	f, exists, err := racikan.Load(path)
	if err != nil {
		log.Printf("copilotd: racikan model diabaikan, setelan .env dipakai — %v", err)
		s.racikanErr = err.Error()
		return
	}
	if exists {
		s.racikan = f
		s.racikanStored = true
	}
}

// TierModels memberi model efektif tiap tier: .env yang ditimpa racikan admin.
func (s *Service) TierModels() racikan.Models {
	s.racikanMu.RLock()
	defer s.racikanMu.RUnlock()
	return s.racikan.Override.Apply(racikan.FromConfig(s.cfg))
}

// Racikan melaporkan racikan model untuk admin.
func (s *Service) Racikan() RacikanState {
	s.racikanMu.RLock()
	defer s.racikanMu.RUnlock()
	env := racikan.FromConfig(s.cfg)
	return RacikanState{
		Effective:  s.racikan.Override.Apply(env),
		Env:        env,
		Override:   s.racikan.Override,
		UpdatedAt:  s.racikan.UpdatedAt,
		UpdatedBy:  s.racikan.UpdatedBy,
		Stored:     s.racikanStored,
		Error:      s.racikanErr,
		Persistent: s.cfg.StateDir != "",
	}
}

// SetRacikan mengganti seluruh isian admin (field nil = ikut .env) dan menyimpannya; berlaku untuk permintaan
// berikutnya tanpa restart. Permintaan yang sedang berjalan tetap memakai model yang sudah dipilih.
func (s *Service) SetRacikan(o racikan.Override, actor string) (RacikanState, error) {
	if err := o.Validate(); err != nil {
		return RacikanState{}, err
	}
	path := racikan.PathIn(s.cfg.StateDir)
	if path == "" {
		return RacikanState{}, ErrRacikanReadOnly
	}
	now := time.Now().UTC()
	f := racikan.File{Override: o, UpdatedAt: &now, UpdatedBy: actor}
	s.racikanMu.Lock()
	if err := racikan.Save(path, f); err != nil {
		s.racikanMu.Unlock()
		return RacikanState{}, err
	}
	s.racikan = f
	s.racikanStored = true
	s.racikanErr = ""
	s.racikanMu.Unlock()
	log.Printf("copilotd: racikan model diganti oleh %s", actor)
	return s.Racikan(), nil
}
