package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// fakeRuntimes menggantikan start dan stop runtime: client SDK yang tidak pernah dinyalakan, dengan daftar model
// yang berlaku saat "runtime" itu dibuat.
type fakeRuntimes struct {
	mu       sync.Mutex
	clock    time.Time
	attempts int
	err      error
	models   []string
	started  []*copilot.Client
	apis     []*rpc.ServerRPC
	stopped  []*copilot.Client
}

func (f *fakeRuntimes) spawn(context.Context, config.Config, string) (*copilot.Client, *rpc.ServerRPC, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.err != nil {
		return nil, nil, f.err
	}
	ids := slices.Clone(f.models)
	client := copilot.NewClient(&copilot.ClientOptions{
		OnListModels: func(context.Context) ([]copilot.ModelInfo, error) {
			out := make([]copilot.ModelInfo, 0, len(ids))
			for _, id := range ids {
				out = append(out, copilot.ModelInfo{ID: id, Name: id})
			}
			return out, nil
		},
	})
	api := &rpc.ServerRPC{}
	f.started, f.apis = append(f.started, client), append(f.apis, api)
	return client, api, nil
}

func (f *fakeRuntimes) halt(c *copilot.Client) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, c)
}

func (f *fakeRuntimes) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fakeRuntimes) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

func (f *fakeRuntimes) set(err error, models ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	if models != nil {
		f.models = models
	}
}

func (f *fakeRuntimes) count() (attempts, stopped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts, len(f.stopped)
}

func (f *fakeRuntimes) client(i int) *copilot.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[i]
}

// wasStopped menunggu sebentar karena runtime lama dimatikan di latar.
func (f *fakeRuntimes) wasStopped(c *copilot.Client) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		found := slices.Contains(f.stopped, c)
		f.mu.Unlock()
		if found || time.Now().After(deadline) {
			return found
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recyclePool(cfg config.Config) (*Pool, *fakeRuntimes) {
	f := &fakeRuntimes{clock: time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC), models: []string{"gpt-6.1-sol"}}
	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = 2
	}
	p := NewPool(cfg)
	wire(p, f)
	return p, f
}

func wire(p *Pool, f *fakeRuntimes) {
	p.spawn, p.now = f.spawn, f.now
	p.identify = nil
	for _, a := range p.accounts {
		a.stop = f.halt
	}
}

func TestRecycleInstallsNewRuntimeAndKeepsOldOneOnFailure(t *testing.T) {
	p, f := recyclePool(config.Config{})
	ctx := context.Background()
	acc := p.accounts[0]
	first, started, err := p.ensure(ctx, acc)
	if err != nil || !started {
		t.Fatalf("ensure = %v, started %v", err, started)
	}

	if done, err := p.recycle(ctx, acc, recycleMinAge); done || err != nil {
		t.Fatalf("runtime yang baru menyala tidak perlu diganti: done %v, err %v", done, err)
	}

	f.advance(2 * time.Minute)
	acc.noModel = map[string]time.Time{"gpt-6.2-sol": f.now().Add(time.Hour)}
	done, err := p.recycle(ctx, acc, recycleMinAge)
	if !done || err != nil {
		t.Fatalf("recycle = %v, %v", done, err)
	}
	second := acc.current()
	if second == first || second != f.client(1) {
		t.Fatal("runtime baru harus terpasang")
	}
	if !f.wasStopped(first) {
		t.Fatal("runtime lama harus dimatikan saat akun menganggur")
	}
	st := p.Status()[0]
	if st.RuntimeStartedAt == nil || !st.RuntimeStartedAt.Equal(f.now()) || len(st.ModelsUnavailable) != 0 {
		t.Fatalf("status setelah diganti = %+v", st)
	}

	f.advance(2 * time.Minute)
	f.set(errors.New("runtime Copilot gagal start: exec: not found"))
	if done, err := p.recycle(ctx, acc, recycleMinAge); done || err == nil {
		t.Fatalf("start gagal: done %v, err %v", done, err)
	}
	if _, stopped := f.count(); acc.current() != second || stopped != 1 {
		t.Fatalf("runtime lama harus tetap terpasang dan hidup bila penggantinya gagal start (dimatikan: %d)", stopped)
	}
}

func TestRecycleLetsRunningRequestsFinishOnOldRuntime(t *testing.T) {
	p, f := recyclePool(config.Config{})
	ctx := context.Background()
	running, err := p.AcquireModel(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	old := running.Client

	f.advance(2 * time.Minute)
	if done, err := p.recycle(ctx, p.accounts[0], recycleMinAge); !done || err != nil {
		t.Fatalf("recycle = %v, %v", done, err)
	}
	next, err := p.AcquireModel(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Client == old {
		t.Fatal("permintaan baru harus memakai runtime baru")
	}

	// Koneksi runtime lama putus: runtime baru yang sedang dipakai permintaan lain tidak boleh ikut dibuang.
	running.Failed(errors.New("connection reset by peer"))
	if p.accounts[0].current() != next.Client {
		t.Fatal("galat koneksi runtime lama membuang runtime baru")
	}

	running.Release()
	time.Sleep(20 * time.Millisecond)
	if _, stopped := f.count(); stopped != 0 {
		t.Fatal("runtime lama dimatikan padahal akun belum menganggur")
	}
	next.Release()
	if !f.wasStopped(old) {
		t.Fatal("runtime lama harus dimatikan setelah permintaan terakhir selesai")
	}
	if _, stopped := f.count(); stopped != 1 {
		t.Fatalf("hanya runtime lama yang dimatikan, dapat %d", stopped)
	}
}

func TestRecycleAgedFollowsMaxAgeAndBacksOffAfterFailure(t *testing.T) {
	p, f := recyclePool(config.Config{RuntimeMaxAge: 6 * time.Hour})
	acc := p.accounts[0]
	if _, _, err := p.ensure(context.Background(), acc); err != nil {
		t.Fatal(err)
	}

	f.advance(5 * time.Hour)
	p.recycleAged()
	if attempts, _ := f.count(); attempts != 1 {
		t.Fatalf("runtime belum melewati batas umur: %d start", attempts)
	}

	f.advance(2 * time.Hour)
	p.recycleAged()
	if attempts, _ := f.count(); attempts != 2 || acc.current() != f.client(1) {
		t.Fatalf("runtime lewat batas umur harus diganti: %d start", attempts)
	}

	f.advance(7 * time.Hour)
	f.set(errors.New("runtime Copilot gagal start: exec: not found"))
	p.recycleAged()
	p.recycleAged()
	if attempts, _ := f.count(); attempts != 3 {
		t.Fatalf("setelah gagal tidak boleh langsung dicoba lagi: %d start", attempts)
	}
	f.advance(recycleRetryAfter + time.Second)
	f.set(nil)
	p.recycleAged()
	if attempts, _ := f.count(); attempts != 4 || acc.current() != f.client(2) || !f.wasStopped(f.client(1)) {
		t.Fatalf("setelah jeda harus dicoba lagi: %d start", attempts)
	}

	off, g := recyclePool(config.Config{})
	if _, _, err := off.ensure(context.Background(), off.accounts[0]); err != nil {
		t.Fatal(err)
	}
	g.advance(1000 * time.Hour)
	off.recycleAged()
	if attempts, _ := g.count(); attempts != 1 {
		t.Fatal("RuntimeMaxAge 0: runtime tidak pernah diganti otomatis")
	}
}

func TestRefreshReadsQuotaThroughNewRuntime(t *testing.T) {
	p, f := recyclePool(config.Config{QuotaCheckInterval: time.Hour})
	// Tiap runtime hanya melaporkan kuota yang dibacanya saat menyala.
	p.fetch = func(_ context.Context, api *rpc.ServerRPC, _ string) (*QuotaInfo, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		used := int64(100 * (slices.Index(f.apis, api) + 1))
		return &QuotaInfo{Type: "premium_interactions", RemainingPercent: 50, Entitlement: 1000, Used: used}, nil
	}
	ctx := context.Background()
	used := func() int64 {
		t.Helper()
		if err := p.Refresh(ctx, ""); err != nil {
			t.Fatal(err)
		}
		st := p.Status()[0]
		if st.Quota == nil {
			t.Fatalf("kuota tidak terbaca: %+v", st)
		}
		return st.Quota.Used
	}

	if got := used(); got != 100 {
		t.Fatalf("pembacaan pertama = %d", got)
	}
	f.advance(2 * time.Minute)
	if got := used(); got != 200 {
		t.Fatalf("Refresh harus membaca lewat runtime baru, dapat %d", got)
	}

	f.advance(2 * time.Minute)
	f.set(errors.New("runtime Copilot gagal start: exec: not found"))
	if got := used(); got != 200 {
		t.Fatalf("data terakhir harus dipertahankan, dapat %d", got)
	}
	if st := p.Status()[0]; !strings.Contains(st.QuotaError, "runtime baru gagal start") {
		t.Fatalf("kuota yang tidak bisa diperbarui harus terlihat sebagai galat: %q", st.QuotaError)
	}
}

func TestModelsRefreshReadsListFromNewRuntime(t *testing.T) {
	f := &fakeRuntimes{clock: time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC), models: []string{"gpt-6.1-sol"}}
	svc := NewServiceWithOptions(config.Config{MaxConcurrency: 2}, Options{Spawn: f.spawn})
	wire(svc.pool, f)
	ctx := context.Background()
	ids := func(list ModelList) string {
		out := make([]string, 0, len(list.Models))
		for _, m := range list.Models {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}

	first, err := svc.Models(ctx, false)
	if err != nil || ids(first) != "gpt-6.1-sol" || !first.ReadAt.Equal(f.now()) || !first.RuntimeStartedAt.Equal(f.now()) {
		t.Fatalf("daftar awal = %q, %+v, %v", ids(first), first, err)
	}

	// Model baru muncul di akun: runtime yang sudah hidup tetap melaporkan daftar lama.
	f.set(nil, "gpt-6.1-sol", "gpt-6.2-sol")
	f.advance(2 * time.Hour)
	stale, err := svc.Models(ctx, false)
	if err != nil || ids(stale) != "gpt-6.1-sol" || !stale.ReadAt.Equal(first.ReadAt) {
		t.Fatalf("tanpa refresh = %q dibaca %s, %v", ids(stale), stale.ReadAt, err)
	}

	fresh, err := svc.Models(ctx, true)
	if err != nil || ids(fresh) != "gpt-6.1-sol,gpt-6.2-sol" || !fresh.ReadAt.Equal(f.now()) || fresh.RefreshError != "" {
		t.Fatalf("refresh = %q, %+v, %v", ids(fresh), fresh, err)
	}

	f.advance(2 * time.Hour)
	f.set(errors.New("runtime Copilot gagal start: exec: not found"), "gpt-6.2-astra")
	failed, err := svc.Models(ctx, true)
	if err != nil || ids(failed) != ids(fresh) || failed.RefreshError == "" || !failed.ReadAt.Equal(fresh.ReadAt) {
		t.Fatalf("refresh gagal harus mengembalikan daftar lama beserta galatnya: %q, %+v, %v", ids(failed), failed, err)
	}
}
