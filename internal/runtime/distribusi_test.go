package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
)

func strategyPool(strategy string, tokens ...string) *Pool {
	return NewPool(config.Config{GitHubTokens: tokens, MaxConcurrency: 1, QuotaMinPercent: 5,
		QuotaCheckInterval: time.Hour, PoolStrategy: strategy})
}

func setUnits(a *account, remainingPercent float64, entitlement int64) {
	a.quota = &QuotaInfo{Type: "premium_interactions", RemainingPercent: remainingPercent, Entitlement: entitlement,
		Used: entitlement - int64(float64(entitlement)*remainingPercent/100)}
}

// pickCounts mengambil n slot berturut-turut (dilepas lagi tiap kali) dan menghitung pemakaian per akun.
func pickCounts(t *testing.T, p *Pool, n int) map[*account]int {
	t.Helper()
	counts := map[*account]int{}
	for i := 0; i < n; i++ {
		acc, err := p.pick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		counts[acc]++
		p.releaseSlot(acc)
	}
	return counts
}

func TestProportionalFollowsRemainingUnits(t *testing.T) {
	p := strategyPool(akun.StrategyProporsional, "a", "b")
	// Keadaan nyata 19 Okt 2026: #1 sisa 118712 unit (23,7%), #2 sisa 500000 unit (100%).
	setUnits(p.accounts[0], 23.7424, 500000)
	setUnits(p.accounts[1], 100, 500000)
	counts := pickCounts(t, p, 1000)
	first, second := counts[p.accounts[0]], counts[p.accounts[1]]
	if first+second != 1000 || first < 185 || first > 200 {
		t.Fatalf("porsi #1 = %d/1000, ingin ≈192 (19,2%%); #2 = %d", first, second)
	}
}

func TestProportionalSpreadsWhenPreferredAccountBusy(t *testing.T) {
	p := strategyPool(akun.StrategyProporsional, "a", "b")
	setUnits(p.accounts[0], 10, 500000)
	setUnits(p.accounts[1], 100, 500000)
	busy, err := p.pick(context.Background())
	if err != nil || busy != p.accounts[1] {
		t.Fatalf("pick pertama = %v (%v), ingin #2 yang sisa kuotanya terbesar", busy, err)
	}
	next, err := p.pick(context.Background())
	if err != nil || next != p.accounts[0] {
		t.Fatalf("saat #2 penuh, permintaan berikutnya harus ke #1, dapat %v (%v)", next, err)
	}
}

func TestUnknownQuotaGetsAverageWeight(t *testing.T) {
	p := strategyPool(akun.StrategyProporsional, "a", "b", "c")
	setUnits(p.accounts[0], 100, 100)
	setUnits(p.accounts[1], 100, 300)
	counts := pickCounts(t, p, 600)
	// Bobot 100 : 300 : rata-rata(200) → 100 : 300 : 200.
	if c := counts[p.accounts[2]]; c < 190 || c > 210 {
		t.Fatalf("akun tanpa data kuota mendapat %d/600, ingin ≈200 (bobot rata-rata)", c)
	}
}

func TestRoundRobinAlternates(t *testing.T) {
	p := strategyPool(akun.StrategyBergiliran, "a", "b")
	setUnits(p.accounts[0], 5.5, 500000)
	setUnits(p.accounts[1], 100, 500000)
	var order []*account
	for i := 0; i < 6; i++ {
		acc, err := p.pick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, acc)
		p.releaseSlot(acc)
	}
	for i := 1; i < len(order); i++ {
		if order[i] == order[i-1] {
			t.Fatalf("bergiliran: pick ke-%d mengulang akun yang sama", i+1)
		}
	}
}

func TestPriorityAlwaysFirstFree(t *testing.T) {
	p := strategyPool(akun.StrategyPrioritas, "a", "b")
	setUnits(p.accounts[0], 10, 500000)
	setUnits(p.accounts[1], 100, 500000)
	if counts := pickCounts(t, p, 20); counts[p.accounts[0]] != 20 {
		t.Fatalf("prioritas: #1 harus selalu dipakai saat longgar, dapat %v", counts)
	}
}

func TestReserveUsedOnlyWhenMainBusyOrUnavailable(t *testing.T) {
	p := strategyPool(akun.StrategyProporsional, "a", "b")
	p.accounts[1].role = akun.RoleCadangan
	setUnits(p.accounts[0], 10, 100)
	setUnits(p.accounts[1], 100, 100000)
	if counts := pickCounts(t, p, 10); counts[p.accounts[0]] != 10 {
		t.Fatalf("cadangan tidak boleh dipakai selama akun utama longgar: %v", counts)
	}
	first, _ := p.pick(context.Background())
	second, err := p.pick(context.Background())
	if first != p.accounts[0] || second != p.accounts[1] || err != nil {
		t.Fatalf("akun utama penuh: cadangan harus dipakai, dapat %v/%v (%v)", first, second, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := p.pick(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("semua penuh: pick = %v, ingin menunggu", err)
	}
	p.releaseSlot(first)
	p.releaseSlot(second)

	// Akun utama hampir habis: cadangan sehat didahulukan.
	setUnits(p.accounts[0], 3, 100)
	if acc, _ := p.pick(context.Background()); acc != p.accounts[1] {
		t.Fatalf("akun utama hampir habis: ingin cadangan, dapat #%d", acc.index)
	}
}

func TestDisabledAccountsNeverPicked(t *testing.T) {
	p := strategyPool(akun.StrategyProporsional, "a", "b")
	p.accounts[0].role = akun.RoleNonaktif
	if counts := pickCounts(t, p, 5); counts[p.accounts[1]] != 5 {
		t.Fatalf("akun nonaktif terpakai: %v", counts)
	}
	p.accounts[1].role = akun.RoleNonaktif
	if _, err := p.pick(context.Background()); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("semua nonaktif: pick = %v, ingin ErrNoAccount", err)
	}
	if p.HasAvailable() {
		t.Fatal("semua nonaktif: HasAvailable harus false")
	}
}

func TestPickWakesWhenSlotReleased(t *testing.T) {
	p := strategyPool(akun.StrategyProporsional, "a")
	held, err := p.pick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *account, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		acc, _ := p.pick(ctx)
		got <- acc
	}()
	time.Sleep(30 * time.Millisecond)
	start := time.Now()
	p.releaseSlot(held)
	select {
	case acc := <-got:
		if acc != held || time.Since(start) > time.Second {
			t.Fatalf("pick yang menunggu = %v setelah %s", acc, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pick yang menunggu tidak bangun setelah slot lepas")
	}
}

func TestNewPoolReadsAccountFileOverEnv(t *testing.T) {
	dir := t.TempDir()
	f := akun.FromTokens([]string{"login", "github_pat_berkas"}, akun.StrategyBergiliran, time.Now())
	f.Accounts[1].Role = akun.RoleCadangan
	f.Accounts[1].Label = "Lab riset"
	if err := akun.Save(akun.PathIn(dir), f, false); err != nil {
		t.Fatal(err)
	}
	p := NewPool(config.Config{StateDir: dir, GitHubTokens: []string{"github_pat_env_diabaikan"}, MaxConcurrency: 2})
	if p.Size() != 2 || p.Strategy() != akun.StrategyBergiliran || p.Source() != akun.PathIn(dir) {
		t.Fatalf("pool dari berkas: size=%d strategi=%s sumber=%s", p.Size(), p.Strategy(), p.Source())
	}
	st := p.Status()
	if st[0].ID != akun.LoginID || st[0].Kind != "login" || st[1].Role != akun.RoleCadangan || st[1].Label != "Lab riset" {
		t.Fatalf("status = %+v", st)
	}

	// Berkas rusak: tetap jalan dengan akun .env.
	if err := os.WriteFile(filepath.Join(dir, akun.FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if q := NewPool(config.Config{StateDir: dir, GitHubTokens: []string{"github_pat_env"}, MaxConcurrency: 1}); q.Size() != 1 || q.Source() != ".env" {
		t.Fatalf("berkas rusak harus kembali ke .env: size=%d sumber=%s", q.Size(), q.Source())
	}
}

func TestIsAuthErrorAvoidsBareNumbers(t *testing.T) {
	for _, msg := range []string{"authentication: Bad credentials", "authorization: no Copilot seat", "401 Unauthorized",
		"403 Forbidden"} {
		if !isAuthError(errors.New(msg)) {
			t.Errorf("isAuthError(%q) = false", msg)
		}
	}
	for _, msg := range []string{"context length 140128 melebihi batas", "query gagal", "rate_limit: slow down"} {
		if isAuthError(errors.New(msg)) {
			t.Errorf("isAuthError(%q) = true", msg)
		}
	}
}

func TestLeaseFailedAuthUsesMediumCooldown(t *testing.T) {
	p := strategyPool(akun.StrategyProporsional, "a", "b")
	acc := p.accounts[0]
	(&Lease{pool: p, acc: acc}).Failed(errors.New("authentication: token revoked"))
	if acc.limitKind != "auth" || !acc.inCooldown(time.Now()) {
		t.Fatalf("token ditolak harus cooldown jenis auth, dapat %q", acc.limitKind)
	}
	if acc2, _ := p.pick(context.Background()); acc2 != p.accounts[1] {
		t.Fatal("akun yang tokennya ditolak harus dilewati")
	}
}
