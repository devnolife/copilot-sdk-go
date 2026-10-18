package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// 19 Okt 2026 07.00 WITA.
var fixedNow = time.Date(2026, 10, 18, 23, 0, 0, 0, time.UTC)

func usagePool(t *testing.T) (*Pool, string) {
	t.Helper()
	dir := t.TempDir()
	p := NewPool(config.Config{StateDir: dir, GitHubTokens: []string{"login", "github_pat_dua"}, MaxConcurrency: 2,
		QuotaMinPercent: 5, QuotaCheckInterval: time.Hour, TimeZone: "Asia/Makassar"})
	p.now = func() time.Time { return fixedNow }
	return p, dir
}

func TestLeaseDoneRecordsUsagePerAccountAndDay(t *testing.T) {
	p, dir := usagePool(t)
	acc := p.accounts[0]
	ok := &Lease{pool: p, acc: acc, started: fixedNow.Add(-2 * time.Second)}
	ok.Done(nil, []UsageCall{{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 7, Cost: 1, NanoAiu: 5e8},
		{InputTokens: 50, OutputTokens: 5}})
	ok.Done(errors.New("tidak boleh dihitung dua kali"), nil)
	(&Lease{pool: p, acc: acc, started: fixedNow}).Done(errors.New("retryable: rate_limit: slow down"), nil)
	(&Lease{pool: p, acc: acc, started: fixedNow}).Done(errors.New("quota: premium request allowance exceeded"), nil)

	ov := p.Overview(7)
	d := ov.Accounts[0]
	today := d.Usage.Today
	if today.Requests != 3 || today.OK != 1 || today.Failed != 2 || today.RateLimited != 1 || today.QuotaExhausted != 1 ||
		today.ModelCalls != 2 || today.InputTokens != 150 || today.OutputTokens != 25 || today.CacheReadTokens != 7 ||
		today.Cost != 1 || today.NanoAiu != 5e8 || today.DurationMs != 2000 {
		t.Fatalf("pemakaian hari ini = %+v", today)
	}
	if len(d.Daily) != 7 || d.Daily[6].Date != "2026-10-19" || d.Daily[6].Requests != 3 || d.Daily[0].Date != "2026-10-13" {
		t.Fatalf("seri harian = %+v", d.Daily)
	}
	if d.Usage.Share7dPercent != 100 || ov.Accounts[1].Usage.Share7dPercent != 0 || ov.Totals.RequestsToday != 3 ||
		ov.Totals.Requests7d != 3 || d.LastUsedAt == nil || d.LastError == "" {
		t.Fatalf("ringkasan = %+v / totals %+v", d, ov.Totals)
	}

	if err := p.usage.flush(fixedNow); err != nil {
		t.Fatal(err)
	}
	path := usagePathIn(dir)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pemakaian.json harus 0600: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "github_pat_dua") {
		t.Fatal("pemakaian.json memuat token")
	}
	reloaded := newUsageBook(path, "Asia/Makassar")
	if s, _, _ := reloaded.summary(acc.id, fixedNow, 1, nil); s.Today.Requests != 3 {
		t.Fatalf("muat ulang pemakaian: %+v", s.Today)
	}
}

func TestUsageDayFollowsTimeZoneAndPrunes(t *testing.T) {
	b := newUsageBook("", "Asia/Makassar")
	// 16.30 UTC = 00.30 WITA keesokan harinya.
	b.record("x", "", "", time.Date(2026, 10, 18, 16, 30, 0, 0, time.UTC), UsageCounts{Requests: 1})
	b.record("x", "", "", fixedNow.AddDate(0, 0, -100), UsageCounts{Requests: 1})
	if _, ok := b.data.Accounts["x"].Days["2026-10-19"]; !ok {
		t.Fatalf("hari WITA salah: %v", b.data.Accounts["x"].Days)
	}
	b.pruneLocked(fixedNow)
	if len(b.data.Accounts["x"].Days) != 1 {
		t.Fatalf("catatan > 90 hari harus dibuang: %v", b.data.Accounts["x"].Days)
	}
	if bad := newUsageBook("", "Zona/Asing"); bad.zone != "Asia/Makassar" {
		t.Fatalf("zona asing harus jatuh ke Asia/Makassar, dapat %s", bad.zone)
	}
}

func TestNoteQuotaKeepsSpacedPointsPlusLatest(t *testing.T) {
	b := newUsageBook("", "Asia/Makassar")
	q := func(used int64) *QuotaInfo {
		return &QuotaInfo{Type: "premium_interactions", Used: used, Entitlement: 500000, RemainingPercent: 100 - float64(used)/5000}
	}
	t0 := fixedNow
	b.noteQuota("x", t0, q(100))
	b.noteQuota("x", t0.Add(10*time.Minute), q(110))
	b.noteQuota("x", t0.Add(20*time.Minute), q(120)) // titik kedua masih muda: diganti
	if pts := b.data.Accounts["x"].Quota; len(pts) != 2 || pts[1].Used != 120 {
		t.Fatalf("titik = %+v", pts)
	}
	b.noteQuota("x", t0.Add(40*time.Minute), q(130)) // titik kedua bergeser ke 40 menit: kini berjarak >= 30 menit
	b.noteQuota("x", t0.Add(50*time.Minute), q(140)) // titik kedua sudah tetap: tambah titik terbaru
	if pts := b.data.Accounts["x"].Quota; len(pts) != 3 || pts[1].Used != 130 || pts[2].Used != 140 {
		t.Fatalf("titik = %+v", pts)
	}
	b.noteQuota("x", t0.Add(51*time.Minute), q(5)) // reset: segmen baru
	if pts := b.data.Accounts["x"].Quota; len(pts) != 4 || pts[3].Used != 5 {
		t.Fatalf("titik = %+v", pts)
	}
	b.noteQuota("x", t0, &QuotaInfo{Unlimited: true})
	if len(b.data.Accounts["x"].Quota) != 4 {
		t.Fatal("kuota tak terbatas tidak dicatat")
	}
}

func TestProjectQuota(t *testing.T) {
	reset := fixedNow.Add(13 * 24 * time.Hour)
	pts := []quotaPoint{
		{At: fixedNow.Add(-48 * time.Hour), Used: 400000, Entitlement: 500000}, // periode lalu
		{At: fixedNow.Add(-24 * time.Hour), Used: 100000, Entitlement: 500000}, // reset → segmen baru
		{At: fixedNow.Add(-12 * time.Hour), Used: 104000, Entitlement: 500000},
		{At: fixedNow, Used: 108000, Entitlement: 500000},
	}
	p := projectQuota(pts, fixedNow, 5, &reset)
	if p == nil || p.Basis != "24h" || p.BurnPerDay != 8000 || p.EmptyAt == nil || p.ThresholdAt == nil {
		t.Fatalf("perkiraan = %+v", p)
	}
	// Sisa 392000 unit, 8000/hari: ambang 5% (25000) setelah 45,875 hari, habis setelah 49 hari.
	if got := p.ThresholdAt.Sub(fixedNow).Hours() / 24; got < 45.8 || got > 45.9 {
		t.Fatalf("ambang tercapai dalam %.2f hari", got)
	}
	if got := p.EmptyAt.Sub(fixedNow).Hours() / 24; got < 48.9 || got > 49.1 || !p.SafeUntilReset {
		t.Fatalf("habis dalam %.2f hari, aman=%v", got, p.SafeUntilReset)
	}

	fast := []quotaPoint{{At: fixedNow.Add(-6 * time.Hour), Used: 300000, Entitlement: 500000}, {At: fixedNow, Used: 400000, Entitlement: 500000}}
	if p := projectQuota(fast, fixedNow, 5, &reset); p == nil || p.SafeUntilReset || p.ThresholdAt.After(reset) {
		t.Fatalf("pemakaian cepat harus habis sebelum reset: %+v", p)
	}
	flat := []quotaPoint{{At: fixedNow.Add(-6 * time.Hour), Used: 1000, Entitlement: 500000}, {At: fixedNow, Used: 1000, Entitlement: 500000}}
	if p := projectQuota(flat, fixedNow, 5, &reset); p == nil || p.BurnPerDay != 0 || p.EmptyAt != nil || !p.SafeUntilReset {
		t.Fatalf("tanpa pemakaian: %+v", p)
	}
	slow := []quotaPoint{{At: fixedNow.Add(-6 * time.Hour), Used: 1000, Entitlement: 500000}, {At: fixedNow, Used: 1000 + 1, Entitlement: 500000}}
	if p := projectQuota(slow, fixedNow, 5, &reset); p == nil || p.EmptyAt != nil || !p.SafeUntilReset {
		t.Fatalf("laju sangat kecil (lebih dari 10 tahun) dianggap aman: %+v", p)
	}
	short := []quotaPoint{{At: fixedNow.Add(-time.Hour), Used: 1, Entitlement: 10}, {At: fixedNow, Used: 2, Entitlement: 10}}
	if projectQuota(short, fixedNow, 5, &reset) != nil {
		t.Fatal("data < 2 jam belum cukup untuk perkiraan")
	}
	if projectQuota(pts, fixedNow.Add(72*time.Hour), 5, &reset) != nil {
		t.Fatal("titik kuota usang (> 48 jam) tidak boleh dipakai")
	}
	week := []quotaPoint{{At: fixedNow.Add(-5 * 24 * time.Hour), Used: 1000, Entitlement: 500000}, {At: fixedNow.Add(-30 * time.Minute), Used: 6000, Entitlement: 500000}, {At: fixedNow, Used: 6010, Entitlement: 500000}}
	if p := projectQuota(week, fixedNow, 5, nil); p == nil || p.Basis != "7d" || p.SafeUntilReset {
		t.Fatalf("24 jam belum cukup: harus memakai 7 hari, tanpa tanggal reset tidak 'aman': %+v", p)
	}
}

func TestOverviewTotalsAndQuotaFromStatus(t *testing.T) {
	p, _ := usagePool(t)
	setUnits(p.accounts[0], 23.7, 500000)
	setUnits(p.accounts[1], 100, 500000)
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p.accounts[0].identity = akun.Identity{Login: "expiredWizard", ResetAt: &reset}
	p.accounts[1].role = akun.RoleCadangan
	ov := p.Overview(30)
	tot := ov.Totals
	if tot.Accounts != 2 || tot.Active != 1 || tot.Reserve != 1 || tot.Disabled != 0 || tot.QuotaKnown != 2 ||
		tot.QuotaEntitlement != 1000000 || tot.QuotaRemaining != 618500 || tot.NextResetAt == nil || !tot.NextResetAt.Equal(reset) {
		t.Fatalf("totals = %+v", tot)
	}
	if ov.Accounts[0].Usage.PeriodStart != "2026-10-01" || ov.Accounts[0].Usage.Period == nil || len(ov.Accounts[0].Daily) != 30 {
		t.Fatalf("periode/seri = %+v", ov.Accounts[0].Usage)
	}
	if ov.Strategy != akun.StrategyProporsional || ov.Source != ".env" || !ov.Persistent || ov.TimeZone != "Asia/Makassar" {
		t.Fatalf("overview = %+v", ov)
	}
}

func TestIdentityFromAuthReadsCopilotUser(t *testing.T) {
	login, plan, reset, yes := "mutiaraaaaaa", "enterprise", "2026-11-01T00:00:00.000Z", true
	id := identityFromAuth(&rpc.TokenAuthInfo{Token: "github_pat_rahasia", CopilotUser: &rpc.CopilotUserResponse{
		Login: &login, CopilotPlan: &plan, QuotaResetDateUTC: &reset, TokenBasedBilling: &yes}}, fixedNow)
	if id == nil || id.Login != login || id.Plan != plan || id.ResetAt == nil ||
		!id.ResetAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || id.TokenBasedBilling == nil || !*id.TokenBasedBilling {
		t.Fatalf("identitas = %+v", id)
	}
	if raw, _ := json.Marshal(id); strings.Contains(string(raw), "rahasia") {
		t.Fatal("identitas tidak boleh menyalin token")
	}
	dateOnly := "2026-12-01"
	user := identityFromAuth(&rpc.UserAuthInfo{Login: "expiredWizard", CopilotUser: &rpc.CopilotUserResponse{QuotaResetDate: &dateOnly}}, fixedNow)
	if user == nil || user.Login != "expiredWizard" || user.ResetAt == nil || user.ResetAt.Day() != 1 || user.ResetAt.Month() != 12 {
		t.Fatalf("identitas login tersimpan = %+v", user)
	}
	if identityFromAuth(&rpc.RawAuthInfoData{}, fixedNow) != nil || identityFromAuth(&rpc.TokenAuthInfo{}, fixedNow) != nil {
		t.Fatal("tanpa data akun harus nil")
	}
}

func TestApplyIdentityPersistsOnlyWhenChanged(t *testing.T) {
	p, dir := usagePool(t)
	if err := p.SetStrategy(akun.StrategyProporsional, ""); err != nil { // tulis akun.json
		t.Fatal(err)
	}
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p.applyIdentity(p.accounts[1], &akun.Identity{Login: "mutia", ResetAt: &reset})
	f, _, _ := akun.Load(akun.PathIn(dir))
	if f.Accounts[1].Login != "mutia" || f.Accounts[1].ResetAt == nil {
		t.Fatalf("identitas harus tersimpan di akun.json: %+v", f.Accounts[1])
	}
	info1, _ := os.Stat(akun.PathIn(dir))
	time.Sleep(10 * time.Millisecond)
	p.applyIdentity(p.accounts[1], &akun.Identity{Login: "", ResetAt: &reset}) // login kosong: login lama dipertahankan
	info2, _ := os.Stat(akun.PathIn(dir))
	if p.accounts[1].identity.Login != "mutia" || !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatal("identitas yang sama tidak boleh menulis ulang akun.json")
	}
}
