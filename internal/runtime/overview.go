package runtime

import (
	"log"
	"math"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
)

// Overview adalah laporan pool untuk halaman admin (GET /v1/accounts); token tidak pernah ikut.
type Overview struct {
	Strategy        string           `json:"strategy"`
	Strategies      []string         `json:"strategies"`
	Roles           []string         `json:"roles"`
	Source          string           `json:"source"`                // "akun.json" | ".env"
	Persistent      bool             `json:"persistent"`            // perubahan admin tersimpan ke berkas
	StoreError      string           `json:"store_error,omitempty"` // akun.json tidak terbaca: perubahan ditolak
	QuotaMinPercent float64          `json:"quota_min_percent"`
	QuotaCheckSec   int              `json:"quota_check_sec"`
	MaxConcurrency  int              `json:"max_concurrency"`
	TimeZone        string           `json:"time_zone"`
	GeneratedAt     time.Time        `json:"generated_at"`
	Days            int              `json:"days"`
	Totals          OverviewTotals   `json:"totals"`
	Accounts        []AccountDetail  `json:"accounts"`
	Removed         []RemovedAccount `json:"removed"`
}

// OverviewTotals adalah ringkasan seluruh pool.
type OverviewTotals struct {
	Accounts      int   `json:"accounts"`
	Active        int   `json:"active"`
	Reserve       int   `json:"reserve"`
	Disabled      int   `json:"disabled"`
	Attention     int   `json:"attention"`
	RequestsToday int64 `json:"requests_today"`
	Requests7d    int64 `json:"requests_7d"`
	// QuotaRemaining/QuotaEntitlement menjumlahkan unit kuota akun yang tidak nonaktif dan kuotanya terbaca.
	QuotaRemaining   int64      `json:"quota_remaining"`
	QuotaEntitlement int64      `json:"quota_entitlement"`
	QuotaKnown       int        `json:"quota_known"`
	NextResetAt      *time.Time `json:"next_reset_at,omitempty"`
}

// AccountDetail adalah keadaan, pemakaian, dan perkiraan satu akun.
type AccountDetail struct {
	AccountStatus
	Projection *Projection  `json:"projection,omitempty"`
	Usage      UsageSummary `json:"usage"`
	Daily      []DailyUsage `json:"daily"`
}

// Overview menyusun laporan pool dengan seri harian `days` hari (1..90).
func (p *Pool) Overview(days int) Overview {
	days = min(max(days, 1), usageKeepDays)
	now := p.now()
	statuses := p.Status()
	p.mu.RLock()
	strategy, managed, persistent, storeErr := p.strategy, p.managed, p.storePath != "", p.storeErr
	p.mu.RUnlock()
	source := ".env"
	if managed {
		source = akun.FileName
	}
	ov := Overview{
		Strategy: strategy, Strategies: akun.Strategies, Roles: akun.Roles, Source: source, Persistent: persistent,
		StoreError:      storeErr,
		QuotaMinPercent: p.cfg.QuotaMinPercent, QuotaCheckSec: int(p.cfg.QuotaCheckInterval / time.Second),
		MaxConcurrency: max(1, p.cfg.MaxConcurrency), TimeZone: p.usage.zone, GeneratedAt: now.UTC(), Days: days,
		Accounts: make([]AccountDetail, 0, len(statuses)),
	}
	present := map[string]bool{}
	var total7d int64
	t := &ov.Totals
	for _, st := range statuses {
		present[st.ID] = true
		var periodStart *time.Time
		if st.ResetAt != nil {
			s := st.ResetAt.AddDate(0, -1, 0)
			periodStart = &s
		}
		d := AccountDetail{AccountStatus: st}
		var lastUsed *time.Time
		d.Usage, d.Daily, lastUsed = p.usage.summary(st.ID, now, days, periodStart)
		if d.LastUsedAt == nil {
			d.LastUsedAt = lastUsed
		}
		d.Projection = p.usage.projection(st.ID, now, p.cfg.QuotaMinPercent, st.ResetAt)
		ov.Accounts = append(ov.Accounts, d)

		t.Accounts++
		switch st.Role {
		case akun.RoleNonaktif:
			t.Disabled++
		case akun.RoleCadangan:
			t.Reserve++
		default:
			t.Active++
		}
		if st.State == "cooldown" || st.State == "hampir_habis" || st.QuotaError != "" {
			t.Attention++
		}
		t.RequestsToday += d.Usage.Today.Requests
		t.Requests7d += d.Usage.Last7d.Requests
		total7d += d.Usage.Last7d.Requests
		if st.Role == akun.RoleNonaktif {
			continue
		}
		if st.Quota != nil && !st.Quota.Unlimited && st.Quota.Entitlement > 0 {
			units, _ := st.Quota.remainingUnits()
			t.QuotaRemaining += int64(math.Round(units))
			t.QuotaEntitlement += st.Quota.Entitlement
			t.QuotaKnown++
		}
		if st.ResetAt != nil && st.ResetAt.After(now) && (t.NextResetAt == nil || st.ResetAt.Before(*t.NextResetAt)) {
			r := *st.ResetAt
			t.NextResetAt = &r
		}
	}
	if total7d > 0 {
		for i := range ov.Accounts {
			share := 100 * float64(ov.Accounts[i].Usage.Last7d.Requests) / float64(total7d)
			ov.Accounts[i].Usage.Share7dPercent = math.Round(share*10) / 10
		}
	}
	ov.Removed = p.usage.removed(now, days, present)
	if ov.Removed == nil {
		ov.Removed = []RemovedAccount{}
	}
	return ov
}

// run menjalankan pekerjaan latar pool sampai stop ditutup: menulis catatan pemakaian tiap menit dan membaca
// ulang kuota akun yang datanya usang tiap QuotaCheckInterval (tanpa menyalakan runtime baru).
func (p *Pool) run(stop <-chan struct{}) {
	flush := time.NewTicker(usageFlushEvery)
	defer flush.Stop()
	var quota <-chan time.Time
	if p.cfg.QuotaCheckInterval > 0 {
		ticker := time.NewTicker(p.cfg.QuotaCheckInterval)
		defer ticker.Stop()
		quota = ticker.C
	}
	for {
		select {
		case <-stop:
			return
		case <-flush.C:
			if err := p.usage.flush(p.now()); err != nil {
				log.Printf("copilotd: menyimpan catatan pemakaian: %v", err)
			}
		case <-quota:
			p.refreshIdle()
		}
	}
}
