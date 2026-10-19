package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

func quotaPool(minPercent float64, tokens ...string) *Pool {
	return NewPool(config.Config{GitHubTokens: tokens, MaxConcurrency: 1, QuotaMinPercent: minPercent,
		QuotaCheckInterval: time.Hour})
}

func setQuota(a *account, remaining float64) {
	a.quota = &QuotaInfo{Type: "premium_interactions", RemainingPercent: remaining, Entitlement: 100}
}

func TestQuotaFromSnapshotsPicksTightestLimitedType(t *testing.T) {
	snaps := map[string]rpc.AccountQuotaSnapshot{
		"premium_interactions": {RemainingPercentage: 25, UsedRequests: 375000, EntitlementRequests: 500000,
			UsageAllowedWithExhaustedQuota: true},
		"chat":        {IsUnlimitedEntitlement: true, RemainingPercentage: 100},
		"completions": {RemainingPercentage: 1},
	}
	q := quotaFromSnapshots(snaps)
	if q == nil || q.Type != "premium_interactions" || q.RemainingPercent != 25 || !q.OverageAllowed || q.Unlimited {
		t.Fatalf("quota = %+v, ingin premium 25%% (completions diabaikan)", q)
	}
	snaps["chat"] = rpc.AccountQuotaSnapshot{RemainingPercentage: 3}
	if q := quotaFromSnapshots(snaps); q.Type != "chat" || q.RemainingPercent != 3 {
		t.Fatalf("quota = %+v, ingin chat 3%%", q)
	}
	unlimited := map[string]rpc.AccountQuotaSnapshot{"premium_interactions": {IsUnlimitedEntitlement: true}}
	if q := quotaFromSnapshots(unlimited); q == nil || !q.Unlimited || lowQuota(q, 5) {
		t.Fatalf("tak terbatas = %+v", q)
	}
	if quotaFromSnapshots(nil) != nil {
		t.Fatal("tanpa snapshot harus nil")
	}
}

func TestLowQuotaThreshold(t *testing.T) {
	q := &QuotaInfo{RemainingPercent: 5}
	if !lowQuota(q, 5) || lowQuota(q, 4.9) || lowQuota(q, 0) || lowQuota(nil, 5) {
		t.Fatal("ambang hampir-habis salah")
	}
}

func TestTokenKindNeverRevealsToken(t *testing.T) {
	for token, want := range map[string]string{"": "login", "github_pat_x": "pat-berbutir", "gho_x": "oauth",
		"ghu_x": "app-pengguna", "ghp_x": "pat-klasik", "lain": "token"} {
		if got := TokenKind(token); got != want || (token != "" && strings.Contains(got, token)) {
			t.Errorf("TokenKind(%q) = %q, ingin %q", token, got, want)
		}
	}
}

func TestPickPrefersHealthyOverLowQuota(t *testing.T) {
	p := quotaPool(5, "a", "b")
	setQuota(p.accounts[0], 3)
	acc, err := p.pick(context.Background(), "")
	if err != nil || acc != p.accounts[1] {
		t.Fatalf("pick = #%v (%v), ingin #2 karena #1 hampir habis", acc.index, err)
	}
}

func TestPickWaitsForBusyHealthyInsteadOfUsingLowQuota(t *testing.T) {
	p := quotaPool(5, "a", "b")
	p.accounts[0].slots <- struct{}{} // #1 sehat tetapi penuh
	setQuota(p.accounts[1], 2)        // #2 hampir habis dan longgar
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if acc, err := p.pick(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pick = %v/%v, ingin menunggu akun sehat (DeadlineExceeded)", acc, err)
	}
}

func TestPickUsesLowQuotaWhenNoHealthyAccountLeft(t *testing.T) {
	p := quotaPool(5, "a", "b", "c")
	p.accounts[0].markLimited()
	setQuota(p.accounts[1], 2)
	setQuota(p.accounts[2], 4)
	acc, err := p.pick(context.Background(), "")
	if err != nil || acc != p.accounts[2] {
		t.Fatalf("pick = #%v (%v), ingin #3 (sisa kuota terbesar di antara yang hampir habis)", acc.index, err)
	}
	if !p.HasAvailable() {
		t.Fatal("akun hampir habis tetap terhitung tersedia")
	}
	p.accounts[1].markLimited()
	p.accounts[2].markLimited()
	if p.HasAvailable() {
		t.Fatal("semua cooldown: HasAvailable harus false")
	}
}

func TestPickWithThresholdOffIgnoresQuota(t *testing.T) {
	p := quotaPool(0, "a", "b")
	setQuota(p.accounts[0], 0)
	if acc, _ := p.pick(context.Background(), ""); acc != p.accounts[0] {
		t.Fatalf("ambang 0 harus mematikan pengalihan proaktif, dapat #%d", acc.index)
	}
}

func TestLeaseFailedQuotaExhaustedUsesLongCooldown(t *testing.T) {
	p := quotaPool(5, "a")
	acc := p.accounts[0]
	(&Lease{pool: p, acc: acc}).Failed(errors.New("quota: You have exceeded your premium request allowance"))
	if !acc.inCooldown(time.Now()) || acc.limitKind != "quota" {
		t.Fatalf("kuota habis harus cooldown jenis quota, dapat %q", acc.limitKind)
	}
	if remaining := time.Until(acc.cooldownUntil()); remaining < cooldownAfterQuota-time.Minute {
		t.Fatalf("cooldown kuota %s, ingin ≈%s", remaining, cooldownAfterQuota)
	}
	b := newTestPool("b").accounts[0]
	(&Lease{pool: p, acc: b}).Failed(errors.New("rate_limit: Too many requests"))
	if b.limitKind != "rate_limit" || time.Until(b.cooldownUntil()) > cooldownAfterLimit {
		t.Fatalf("rate limit harus cooldown pendek, dapat %q", b.limitKind)
	}
}

func TestMaybeRefreshFetchesInBackgroundAndRespectsInterval(t *testing.T) {
	p := quotaPool(5, "github_pat_rahasia")
	var calls atomic.Int32
	p.fetch = func(ctx context.Context, api *rpc.ServerRPC, _ string) (*QuotaInfo, error) {
		calls.Add(1)
		return &QuotaInfo{Type: "premium_interactions", RemainingPercent: 4, Entitlement: 100, Used: 96}, nil
	}
	acc := p.accounts[0]
	p.maybeRefresh(acc, false)
	if calls.Load() != 0 {
		t.Fatal("runtime belum hidup: kuota tidak boleh dibaca")
	}
	acc.rpcAPI = &rpc.ServerRPC{}
	acc.identity.Login = "akun-uji"
	waitFor := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			acc.mu.Lock()
			done := !acc.refreshing && calls.Load() == n
			acc.mu.Unlock()
			if done {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("panggilan kuota = %d, ingin %d", calls.Load(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	p.maybeRefresh(acc, false)
	waitFor(1)
	p.maybeRefresh(acc, false) // belum lewat interval
	time.Sleep(20 * time.Millisecond)
	waitFor(1)
	p.maybeRefresh(acc, true) // dipaksa, mis. setelah galat kuota
	waitFor(2)

	st := p.Status()[0]
	if st.State != "hampir_habis" || st.Kind != "pat-berbutir" || st.Login != "akun-uji" || st.Quota == nil ||
		st.Quota.RemainingPercent != 4 || st.QuotaCheckedAt == nil {
		t.Fatalf("status = %+v", st)
	}
	raw, _ := json.Marshal(p.Status())
	if strings.Contains(string(raw), "rahasia") {
		t.Fatalf("status memuat token: %s", raw)
	}

	p.fetch = func(ctx context.Context, api *rpc.ServerRPC, _ string) (*QuotaInfo, error) {
		calls.Add(1)
		return nil, errors.New("experimental API tidak tersedia")
	}
	p.maybeRefresh(acc, true)
	waitFor(3)
	if st := p.Status()[0]; st.QuotaError == "" || st.Quota == nil || st.Quota.RemainingPercent != 4 {
		t.Fatalf("galat baca kuota harus mempertahankan data terakhir: %+v", st)
	}
}

func TestStatusReportsCooldown(t *testing.T) {
	p := quotaPool(5, "a")
	p.accounts[0].markQuotaExhausted()
	st := p.Status()[0]
	if st.State != "cooldown" || st.LimitKind != "quota" || st.CooldownUntil == nil || st.Capacity != 1 {
		t.Fatalf("status = %+v", st)
	}
}
