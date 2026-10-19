package runtime

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
	_ "time/tzdata" // zona waktu tetap dikenali walau zoneinfo sistem tidak ada

	"github.com/devnolife/copilot-sdk-go/internal/akun"
)

const (
	usageFileName   = "pemakaian.json"
	usageKeepDays   = 90 // masa simpan catatan pemakaian harian
	quotaKeepDays   = 45 // masa simpan riwayat titik kuota
	quotaPointEvery = 30 * time.Minute
	usageFlushEvery = time.Minute
	dayLayout       = "2006-01-02"

	// minProjectionSpan: rentang data kuota minimal sebelum perkiraan habis dihitung.
	minProjectionSpan = 2 * time.Hour
	// staleProjection: titik kuota terakhir yang lebih tua dari ini tidak dipakai untuk perkiraan.
	staleProjection = 48 * time.Hour
	// maxProjectionDays: perkiraan lebih jauh dari ini dianggap "tidak akan habis".
	maxProjectionDays = 3650
)

// UsageCounts adalah pemakaian lewat copilotd (revisi.studio) pada satu akun dalam satu rentang waktu.
type UsageCounts struct {
	Requests         int64   `json:"requests"`
	OK               int64   `json:"ok"`
	Failed           int64   `json:"failed"`
	RateLimited      int64   `json:"rate_limited"`
	QuotaExhausted   int64   `json:"quota_exhausted"`
	ModelCalls       int64   `json:"model_calls"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	Cost             float64 `json:"cost"`
	NanoAiu          float64 `json:"nano_aiu"`
	DurationMs       int64   `json:"duration_ms"`
}

func (c *UsageCounts) add(o UsageCounts) {
	c.Requests += o.Requests
	c.OK += o.OK
	c.Failed += o.Failed
	c.RateLimited += o.RateLimited
	c.QuotaExhausted += o.QuotaExhausted
	c.ModelCalls += o.ModelCalls
	c.InputTokens += o.InputTokens
	c.OutputTokens += o.OutputTokens
	c.CacheReadTokens += o.CacheReadTokens
	c.CacheWriteTokens += o.CacheWriteTokens
	c.Cost += o.Cost
	c.NanoAiu += o.NanoAiu
	c.DurationMs += o.DurationMs
}

// DailyUsage adalah pemakaian satu hari (tanggal menurut zona waktu catatan).
type DailyUsage struct {
	Date string `json:"date"`
	UsageCounts
}

// UsageSummary meringkas pemakaian satu akun.
type UsageSummary struct {
	Today  UsageCounts `json:"today"`
	Last7d UsageCounts `json:"last_7d"`
	// Period: sejak awal periode kuota berjalan (sebulan sebelum tanggal reset berikutnya), bila tanggal reset
	// diketahui.
	Period         *UsageCounts `json:"period,omitempty"`
	PeriodStart    string       `json:"period_start,omitempty"`
	Share7dPercent float64      `json:"share_7d_percent"`
}

// Projection memperkirakan kapan kuota akun mencapai ambang hampir habis dan habis, dari laju pemakaian seluruh akun
// (termasuk pemakaian di luar revisi.studio) yang teramati lewat titik kuota. Hanya perkiraan.
type Projection struct {
	Basis          string     `json:"basis"`        // "24h" | "7d": rentang data yang dipakai
	BurnPerDay     float64    `json:"burn_per_day"` // unit kuota per hari
	MeasuredAt     time.Time  `json:"measured_at"`
	ThresholdAt    *time.Time `json:"threshold_at,omitempty"`
	EmptyAt        *time.Time `json:"empty_at,omitempty"`
	SafeUntilReset bool       `json:"safe_until_reset"`
}

type quotaPoint struct {
	At               time.Time `json:"at"`
	Used             int64     `json:"used"`
	Entitlement      int64     `json:"entitlement"`
	RemainingPercent float64   `json:"remaining_percent"`
}

type usageAccount struct {
	Login     string                  `json:"login,omitempty"`
	Label     string                  `json:"label,omitempty"`
	RemovedAt *time.Time              `json:"removed_at,omitempty"`
	LastUsed  *time.Time              `json:"last_used_at,omitempty"`
	Days      map[string]*UsageCounts `json:"days"`
	Quota     []quotaPoint            `json:"quota,omitempty"`
}

type usageData struct {
	Version  int                      `json:"version"`
	TimeZone string                   `json:"time_zone"`
	Accounts map[string]*usageAccount `json:"accounts"`
}

// usageBook menyimpan catatan pemakaian per akun di memori dan menuliskannya ke pemakaian.json (0600, tanpa
// token; kunci akun = id sidik).
type usageBook struct {
	mu    sync.Mutex
	path  string
	loc   *time.Location
	zone  string
	data  usageData
	dirty bool
}

func usagePathIn(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, usageFileName)
}

func newUsageBook(path, tz string) *usageBook {
	b := &usageBook{path: path, zone: tz, data: usageData{Version: 1, Accounts: map[string]*usageAccount{}}}
	loc, err := time.LoadLocation(tz)
	if tz == "" || err != nil {
		if tz != "" {
			log.Printf("copilotd: zona waktu %q tidak dikenal; memakai Asia/Makassar", tz)
		}
		loc, b.zone = time.FixedZone("WITA", 8*3600), "Asia/Makassar"
	}
	b.loc = loc
	b.data.TimeZone = b.zone
	if path == "" {
		return b
	}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Printf("copilotd: membaca %s: %v", path, err)
	default:
		var d usageData
		if err := json.Unmarshal(raw, &d); err != nil {
			bad := path + ".rusak-" + time.Now().Format("20060102-150405")
			log.Printf("copilotd: %s rusak (%v); disimpan sebagai %s, catatan dimulai ulang", path, err, bad)
			_ = os.Rename(path, bad)
			return b
		}
		for id, acc := range d.Accounts {
			if acc == nil {
				continue
			}
			if acc.Days == nil {
				acc.Days = map[string]*UsageCounts{}
			}
			b.data.Accounts[id] = acc
		}
	}
	return b
}

func (b *usageBook) day(t time.Time) string { return t.In(b.loc).Format(dayLayout) }

func (b *usageBook) accountLocked(id string) *usageAccount {
	acc := b.data.Accounts[id]
	if acc == nil {
		acc = &usageAccount{Days: map[string]*UsageCounts{}}
		b.data.Accounts[id] = acc
	}
	return acc
}

func (b *usageBook) record(id, login, label string, at time.Time, c UsageCounts) {
	b.mu.Lock()
	defer b.mu.Unlock()
	acc := b.accountLocked(id)
	if login != "" {
		acc.Login = login
	}
	acc.Label = label
	key := b.day(at)
	d := acc.Days[key]
	if d == nil {
		d = &UsageCounts{}
		acc.Days[key] = d
	}
	d.add(c)
	t := at.UTC()
	acc.LastUsed = &t
	b.dirty = true
}

// restore menghapus tanda terhapus (token yang sama ditambahkan lagi). Permintaan yang selesai sesudah akun dihapus
// tidak menghapus tanda itu.
func (b *usageBook) restore(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if acc := b.data.Accounts[id]; acc != nil && acc.RemovedAt != nil {
		acc.RemovedAt = nil
		b.dirty = true
	}
}

// noteQuota menyimpan titik kuota untuk perkiraan habis: titik berjarak >= 30 menit, ditambah satu titik terbaru
// yang terus diperbarui; penurunan pemakaian (reset) atau perubahan entitlement memulai segmen baru.
func (b *usageBook) noteQuota(id string, at time.Time, q *QuotaInfo) {
	if q == nil || q.Unlimited || q.Entitlement <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	acc := b.accountLocked(id)
	pt := quotaPoint{At: at.UTC(), Used: q.Used, Entitlement: q.Entitlement, RemainingPercent: q.RemainingPercent}
	n := len(acc.Quota)
	switch {
	case n < 2:
		acc.Quota = append(acc.Quota, pt)
	case pt.Used < acc.Quota[n-1].Used || pt.Entitlement != acc.Quota[n-1].Entitlement:
		acc.Quota = append(acc.Quota, pt)
	case acc.Quota[n-1].At.Sub(acc.Quota[n-2].At) >= quotaPointEvery:
		acc.Quota = append(acc.Quota, pt)
	default:
		acc.Quota[n-1] = pt
	}
	b.dirty = true
}

func (b *usageBook) setNames(id, login, label string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	acc := b.data.Accounts[id]
	if acc == nil {
		if login == "" && label == "" {
			return
		}
		acc = b.accountLocked(id)
	}
	if login != "" {
		acc.Login = login
	}
	acc.Label = label
	b.dirty = true
}

func (b *usageBook) markRemoved(id, login, label string, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	acc := b.accountLocked(id)
	if login != "" {
		acc.Login = login
	}
	if label != "" {
		acc.Label = label
	}
	t := at.UTC()
	acc.RemovedAt = &t
	b.dirty = true
}

func (b *usageBook) pruneLocked(now time.Time) {
	cutoff := b.day(now.AddDate(0, 0, -(usageKeepDays - 1)))
	quotaCutoff := now.AddDate(0, 0, -quotaKeepDays)
	for id, acc := range b.data.Accounts {
		for key := range acc.Days {
			if key < cutoff {
				delete(acc.Days, key)
			}
		}
		kept := acc.Quota[:0]
		for _, pt := range acc.Quota {
			if !pt.At.Before(quotaCutoff) {
				kept = append(kept, pt)
			}
		}
		acc.Quota = kept
		if acc.RemovedAt != nil && len(acc.Days) == 0 && len(acc.Quota) == 0 {
			delete(b.data.Accounts, id)
		}
	}
}

// flush menulis catatan ke disk bila ada perubahan (berkas 0600, ditulis atomik).
func (b *usageBook) flush(now time.Time) error {
	b.mu.Lock()
	if !b.dirty || b.path == "" {
		b.mu.Unlock()
		return nil
	}
	b.pruneLocked(now)
	raw, err := json.Marshal(b.data)
	b.dirty = false
	b.mu.Unlock()
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(b.path), 0o700); err == nil {
			err = akun.WriteFileAtomic(b.path, raw)
		}
	}
	if err != nil {
		b.mu.Lock()
		b.dirty = true
		b.mu.Unlock()
	}
	return err
}

// summary memberi ringkasan pemakaian, seri harian `days` hari terakhir (lama → baru, hari kosong bernilai nol),
// dan waktu pakai terakhir yang tercatat.
func (b *usageBook) summary(id string, now time.Time, days int, periodStart *time.Time) (UsageSummary, []DailyUsage, *time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	acc := b.data.Accounts[id]
	daily := make([]DailyUsage, 0, days)
	for i := days - 1; i >= 0; i-- {
		key := b.day(now.AddDate(0, 0, -i))
		entry := DailyUsage{Date: key}
		if acc != nil {
			if d := acc.Days[key]; d != nil {
				entry.UsageCounts = *d
			}
		}
		daily = append(daily, entry)
	}
	var s UsageSummary
	if periodStart != nil {
		s.PeriodStart = b.day(*periodStart)
		s.Period = &UsageCounts{}
	}
	if acc == nil {
		return s, daily, nil
	}
	today := b.day(now)
	weekStart := b.day(now.AddDate(0, 0, -6))
	for key, d := range acc.Days {
		if key > today {
			continue
		}
		if key == today {
			s.Today.add(*d)
		}
		if key >= weekStart {
			s.Last7d.add(*d)
		}
		if s.Period != nil && key >= s.PeriodStart {
			s.Period.add(*d)
		}
	}
	var last *time.Time
	if acc.LastUsed != nil {
		t := *acc.LastUsed
		last = &t
	}
	return s, daily, last
}

// RemovedAccount adalah akun yang sudah keluar dari pool tetapi masih punya pemakaian dalam rentang laporan.
type RemovedAccount struct {
	ID        string       `json:"id"`
	Login     string       `json:"login,omitempty"`
	Label     string       `json:"label,omitempty"`
	RemovedAt *time.Time   `json:"removed_at,omitempty"`
	Usage     UsageSummary `json:"usage"`
	Daily     []DailyUsage `json:"daily"`
}

// removed mengumpulkan catatan akun yang tidak ada lagi di pool dan masih punya pemakaian dalam `days` hari.
func (b *usageBook) removed(now time.Time, days int, present map[string]bool) []RemovedAccount {
	b.mu.Lock()
	ids := make([]string, 0)
	for id := range b.data.Accounts {
		if !present[id] {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	slices.Sort(ids)
	var out []RemovedAccount
	for _, id := range ids {
		s, daily, _ := b.summary(id, now, days, nil)
		total := int64(0)
		for _, d := range daily {
			total += d.Requests
		}
		if total == 0 {
			continue
		}
		b.mu.Lock()
		acc := b.data.Accounts[id]
		item := RemovedAccount{ID: id, Usage: s, Daily: daily}
		if acc != nil {
			item.Login, item.Label = acc.Login, acc.Label
			if acc.RemovedAt != nil {
				t := *acc.RemovedAt
				item.RemovedAt = &t
			}
		}
		b.mu.Unlock()
		out = append(out, item)
	}
	return out
}

func (b *usageBook) projection(id string, now time.Time, minPercent float64, resetAt *time.Time) *Projection {
	b.mu.Lock()
	var pts []quotaPoint
	if acc := b.data.Accounts[id]; acc != nil {
		pts = slices.Clone(acc.Quota)
	}
	b.mu.Unlock()
	return projectQuota(pts, now, minPercent, resetAt)
}

// projectQuota menghitung perkiraan dari titik kuota segmen periode berjalan. Laju diambil dari rentang 24 jam
// terakhir, atau 7 hari bila 24 jam belum cukup (minimal minProjectionSpan); nil bila data belum cukup.
func projectQuota(pts []quotaPoint, now time.Time, minPercent float64, resetAt *time.Time) *Projection {
	if len(pts) == 0 {
		return nil
	}
	start := 0
	for i := 1; i < len(pts); i++ {
		if pts[i].Used < pts[i-1].Used || pts[i].Entitlement != pts[i-1].Entitlement {
			start = i
		}
	}
	seg := pts[start:]
	last := seg[len(seg)-1]
	if last.Entitlement <= 0 || now.Sub(last.At) > staleProjection {
		return nil
	}
	var base quotaPoint
	basis := ""
	for _, window := range []struct {
		name string
		span time.Duration
	}{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}} {
		for _, pt := range seg {
			if last.At.Sub(pt.At) <= window.span {
				if last.At.Sub(pt.At) >= minProjectionSpan {
					base, basis = pt, window.name
				}
				break
			}
		}
		if basis != "" {
			break
		}
	}
	if basis == "" {
		return nil
	}
	spanDays := last.At.Sub(base.At).Hours() / 24
	burn := float64(last.Used-base.Used) / spanDays
	proj := &Projection{Basis: basis, BurnPerDay: math.Round(burn*10) / 10, MeasuredAt: last.At}
	if burn <= 0 {
		proj.BurnPerDay = 0
		proj.SafeUntilReset = resetAt != nil
		return proj
	}
	reach := func(units float64) (*time.Time, bool) {
		if units <= 0 {
			t := last.At
			return &t, true
		}
		days := units / burn
		if days > maxProjectionDays {
			return nil, false
		}
		t := last.At.Add(time.Duration(days * 24 * float64(time.Hour)))
		return &t, true
	}
	remaining := float64(last.Entitlement - last.Used)
	empty, emptyOK := reach(remaining)
	proj.EmptyAt = empty
	limit, limitOK := empty, emptyOK
	if minPercent > 0 {
		threshold, ok := reach(remaining - float64(last.Entitlement)*minPercent/100)
		proj.ThresholdAt = threshold
		limit, limitOK = threshold, ok
	}
	proj.SafeUntilReset = resetAt != nil && (!limitOK || limit.After(*resetAt))
	return proj
}
