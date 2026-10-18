package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
)

const newToken = "github_pat_11BARU_rahasia_sekali"

type fakeProber struct {
	calls atomic.Int32
	login string
	err   error
}

func (f *fakeProber) probe(_ context.Context, _ config.Config, token string) (AccountProbe, error) {
	f.calls.Add(1)
	if f.err != nil {
		return AccountProbe{}, f.err
	}
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	yes := true
	return AccountProbe{
		Kind: TokenKind(token), Authenticated: true, Login: f.login, Plan: "enterprise", ResetAt: &reset,
		TokenBasedBilling: &yes,
		Quota:             &QuotaInfo{Type: "premium_interactions", RemainingPercent: 100, Entitlement: 500000},
	}, nil
}

func managedPool(t *testing.T, prober *fakeProber) (*Pool, string) {
	t.Helper()
	dir := t.TempDir()
	p := NewPool(config.Config{StateDir: dir, GitHubTokens: []string{"login"}, MaxConcurrency: 2,
		QuotaMinPercent: 5, QuotaCheckInterval: time.Hour, TimeZone: "Asia/Makassar"})
	p.probe = prober.probe
	return p, dir
}

func TestAddAccountProbesPersistsAndHidesToken(t *testing.T) {
	prober := &fakeProber{login: "mutiaraaaaaa"}
	p, dir := managedPool(t, prober)
	if p.Source() != ".env" {
		t.Fatalf("sebelum perubahan pertama sumber harus .env, dapat %s", p.Source())
	}
	st, err := p.Add(context.Background(), AddInput{Token: "  " + newToken + " ", Label: " Lab  riset ", Actor: "admin:a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != akun.Fingerprint(newToken) || st.Login != "mutiaraaaaaa" || st.Role != akun.RoleUtama ||
		st.Source != akun.SourceAdmin || st.Label != "Lab riset" || st.Plan != "enterprise" || st.ResetAt == nil ||
		st.Quota == nil || st.Index != 2 || st.AddedBy != "admin:a@b.c" {
		t.Fatalf("status akun baru = %+v", st)
	}
	if p.Size() != 2 || p.Source() != akun.PathIn(dir) {
		t.Fatalf("pool setelah tambah: size=%d sumber=%s", p.Size(), p.Source())
	}
	raw, err := os.ReadFile(akun.PathIn(dir))
	if err != nil || !strings.Contains(string(raw), newToken) {
		t.Fatalf("token harus tersimpan di akun.json (0600): %v", err)
	}
	for _, payload := range []any{p.Status(), p.Overview(7)} {
		out, _ := json.Marshal(payload)
		if strings.Contains(string(out), "rahasia") {
			t.Fatalf("respons memuat token: %s", out)
		}
	}

	// Pool baru dari berkas yang sama memuat kedua akun.
	q := NewPool(config.Config{StateDir: dir, MaxConcurrency: 1})
	if q.Size() != 2 || q.Status()[1].Login != "mutiaraaaaaa" {
		t.Fatalf("muat ulang: %+v", q.Status())
	}
}

func TestAddAccountRejectsBadAndDuplicateTokens(t *testing.T) {
	prober := &fakeProber{login: "mutia"}
	p, _ := managedPool(t, prober)
	for token, want := range map[string]error{
		"":                      ErrAccountInvalid,
		"ghp_klasik123":         ErrAccountInvalid,
		"abc123":                ErrAccountInvalid,
		"github_pat_ada spasi":  ErrAccountInvalid,
		strings.Repeat("x", 10): ErrAccountInvalid,
	} {
		if _, err := p.Add(context.Background(), AddInput{Token: token}); !errors.Is(err, want) {
			t.Errorf("Add(%q) = %v, ingin %v", token, err, want)
		}
	}
	if _, err := p.Add(context.Background(), AddInput{Token: newToken, Role: "admin"}); !errors.Is(err, ErrAccountInvalid) {
		t.Errorf("peran asing: %v", err)
	}
	if _, err := p.Add(context.Background(), AddInput{Token: newToken, Label: strings.Repeat("a", 61)}); !errors.Is(err, ErrAccountInvalid) {
		t.Errorf("label terlalu panjang: %v", err)
	}
	if prober.calls.Load() != 0 {
		t.Fatalf("token tidak sah tidak boleh diperiksa ke runtime (%d probe)", prober.calls.Load())
	}
	if _, err := p.Add(context.Background(), AddInput{Token: newToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Add(context.Background(), AddInput{Token: newToken}); !errors.Is(err, ErrAccountDuplicate) {
		t.Fatalf("token sama: %v", err)
	}
	if _, err := p.Add(context.Background(), AddInput{Token: "gho_akun_sama"}); !errors.Is(err, ErrAccountDuplicate) ||
		!strings.Contains(err.Error(), "mutia") {
		t.Fatalf("login sama harus ditolak: %v", err)
	}

	prober.err = errors.New("Not authenticated: token gho_bocor123 ditolak")
	_, err := p.Add(context.Background(), AddInput{Token: "gho_bocor123"})
	if !errors.Is(err, ErrAccountRejected) || strings.Contains(err.Error(), "gho_bocor123") {
		t.Fatalf("galat probe harus ErrAccountRejected tanpa token: %v", err)
	}
	if p.Size() != 2 {
		t.Fatalf("akun gagal tidak boleh masuk pool: size=%d", p.Size())
	}
}

func TestUpdateRemoveAndStrategyKeepOneActiveAccount(t *testing.T) {
	p, dir := managedPool(t, &fakeProber{login: "mutia"})
	added, err := p.Add(context.Background(), AddInput{Token: newToken})
	if err != nil {
		t.Fatal(err)
	}
	nonaktif, cadangan, label := akun.RoleNonaktif, akun.RoleCadangan, "  Akun   kampus "
	if _, err := p.Update("tidakada0000", UpdateInput{Role: &cadangan}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("akun tidak dikenal: %v", err)
	}
	st, err := p.Update(added.ID, UpdateInput{Role: &nonaktif, Label: &label, Actor: "admin:x"})
	if err != nil || st.Role != akun.RoleNonaktif || st.State != "nonaktif" || st.Label != "Akun kampus" {
		t.Fatalf("ubah peran: %+v (%v)", st, err)
	}
	if _, err := p.Update(akun.LoginID, UpdateInput{Role: &nonaktif}); !errors.Is(err, ErrLastActive) {
		t.Fatalf("akun aktif terakhir tidak boleh dinonaktifkan: %v", err)
	}
	if err := p.Remove(akun.LoginID, "admin:x"); !errors.Is(err, ErrLastActive) {
		t.Fatalf("akun aktif terakhir tidak boleh dihapus: %v", err)
	}
	if err := p.SetStrategy("acak", "admin:x"); !errors.Is(err, ErrAccountInvalid) {
		t.Fatalf("strategi asing: %v", err)
	}
	if err := p.SetStrategy(akun.StrategyPrioritas, "admin:x"); err != nil || p.Strategy() != akun.StrategyPrioritas {
		t.Fatalf("ganti strategi: %v", err)
	}
	if err := p.Remove(added.ID, "admin:x"); err != nil || p.Size() != 1 {
		t.Fatalf("hapus akun nonaktif: %v (size %d)", err, p.Size())
	}
	if err := p.Remove(added.ID, "admin:x"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("hapus dua kali: %v", err)
	}
	f, exists, err := akun.Load(akun.PathIn(dir))
	if err != nil || !exists || len(f.Accounts) != 1 || f.Strategy != akun.StrategyPrioritas {
		t.Fatalf("akun.json setelah perubahan: %+v (%v)", f, err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "cadangan", "akun-*.json"))
	if len(backups) == 0 {
		t.Fatal("perubahan admin harus menyimpan cadangan akun.json")
	}
}

func TestRemoveStopsRuntimeOnlyAfterLastLease(t *testing.T) {
	p, _ := managedPool(t, &fakeProber{login: "mutia"})
	added, err := p.Add(context.Background(), AddInput{Token: newToken})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	acc := p.findLocked(added.ID)
	p.mu.RUnlock()
	acc.slots <- struct{}{} // permintaan sedang berjalan
	if err := p.Remove(added.ID, ""); err != nil {
		t.Fatal(err)
	}
	acc.mu.Lock()
	removed := acc.removed
	acc.mu.Unlock()
	if !removed {
		t.Fatal("akun harus ditandai terhapus")
	}
	for i := 0; i < 5; i++ {
		got, err := p.pick(context.Background())
		if err != nil || got == acc {
			t.Fatalf("akun terhapus masih dipilih: %v", err)
		}
		p.releaseSlot(got)
	}
	p.releaseSlot(acc) // permintaan selesai: runtime dimatikan (client sudah nil di tes ini)
}

func TestUpdateWithoutStateDirStaysInMemory(t *testing.T) {
	p := strategyPool(akun.StrategyProporsional, "github_pat_a", "github_pat_b")
	role := akun.RoleCadangan
	if _, err := p.Update(akun.Fingerprint("github_pat_b"), UpdateInput{Role: &role}); err != nil {
		t.Fatal(err)
	}
	if p.Status()[1].Role != akun.RoleCadangan || p.Source() != ".env" {
		t.Fatalf("tanpa StateDir perubahan hanya di memori: %+v", p.Status()[1])
	}
}
