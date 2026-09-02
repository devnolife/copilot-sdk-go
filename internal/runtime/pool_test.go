package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

func newTestPool(tokens ...string) *Pool {
	return NewPool(config.Config{GitHubTokens: tokens, MaxConcurrency: 1})
}

func TestNewPoolOneAccountPerToken(t *testing.T) {
	p := newTestPool("a", "b", "c")
	if p.Size() != 3 {
		t.Fatalf("Size() = %d, ingin 3", p.Size())
	}
	for i, acc := range p.accounts {
		if acc.index != i+1 {
			t.Errorf("akun ke-%d punya index %d", i, acc.index)
		}
		if cap(acc.slots) != 1 {
			t.Errorf("kapasitas slot akun #%d = %d, ingin MaxConcurrency=1", acc.index, cap(acc.slots))
		}
	}

	// Tanpa token sama sekali tetap ada satu akun (login copilot/gh).
	if got := NewPool(config.Config{MaxConcurrency: 2}).Size(); got != 1 {
		t.Errorf("pool tanpa token Size() = %d, ingin 1", got)
	}
}

func TestPickSpreadsAcrossAccounts(t *testing.T) {
	p := newTestPool("a", "b")
	ctx := context.Background()

	first, err := p.pick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.pick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("dua pick berturut-turut memakai akun yang sama (#%d) padahal slot penuh", first.index)
	}

	// Semua slot terpakai: pick harus menunggu sampai context selesai.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.pick(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pick saat semua penuh = %v, ingin context.DeadlineExceeded", err)
	}
}

func TestPickSkipsAccountInCooldown(t *testing.T) {
	p := newTestPool("a", "b")
	p.accounts[0].markLimited()

	acc, err := p.pick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acc != p.accounts[1] {
		t.Fatalf("pick memilih akun #%d, ingin #2 karena #1 cooldown", acc.index)
	}
}

func TestPickAllInCooldownPrefersEarliestRecovery(t *testing.T) {
	p := newTestPool("a", "b", "c")
	now := time.Now()
	p.accounts[0].cooldownEnd = now.Add(10 * time.Minute)
	p.accounts[1].cooldownEnd = now.Add(1 * time.Minute)
	p.accounts[2].cooldownEnd = now.Add(5 * time.Minute)

	acc, err := p.pick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acc != p.accounts[1] {
		t.Fatalf("pick memilih akun #%d, ingin #2 yang paling cepat pulih", acc.index)
	}
}

func TestPickAllInCooldownAndFullHonoursContext(t *testing.T) {
	p := newTestPool("a")
	p.accounts[0].markLimited()
	p.accounts[0].slots <- struct{}{} // penuh

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.pick(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pick = %v, ingin context.DeadlineExceeded", err)
	}
}

func TestLeaseReleaseIsIdempotent(t *testing.T) {
	p := newTestPool("a")
	acc := p.accounts[0]
	acc.slots <- struct{}{}

	lease := &Lease{pool: p, acc: acc}
	lease.Release()
	if len(acc.slots) != 0 {
		t.Fatalf("slot masih terpakai setelah Release: %d", len(acc.slots))
	}
	// Panggilan kedua tidak boleh memblokir atau panic.
	done := make(chan struct{})
	go func() { lease.Release(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Release kedua memblokir")
	}

	var nilLease *Lease
	nilLease.Release() // aman
}

func TestLeaseFailedClassifiesErrors(t *testing.T) {
	p := newTestPool("a")
	acc := p.accounts[0]
	lease := &Lease{pool: p, acc: acc}

	lease.Failed(nil)
	if acc.inCooldown(time.Now()) {
		t.Fatal("Failed(nil) tidak boleh mengubah apa pun")
	}

	lease.Failed(errors.New("connection reset by peer"))
	if acc.inCooldown(time.Now()) {
		t.Fatal("kesalahan koneksi tidak boleh memicu cooldown")
	}
	if acc.client != nil {
		t.Fatal("kesalahan koneksi harus membuang client")
	}

	lease.Failed(errors.New("HTTP 429 Too Many Requests"))
	if !acc.inCooldown(time.Now()) {
		t.Fatal("rate limit harus menaruh akun dalam cooldown")
	}
	until := acc.cooldownUntil()
	if remaining := time.Until(until); remaining < cooldownAfterLimit-time.Minute || remaining > cooldownAfterLimit {
		t.Fatalf("durasi cooldown %s, ingin ≈%s", remaining, cooldownAfterLimit)
	}

	var nilLease *Lease
	nilLease.Failed(errors.New("x")) // aman
}

func TestErrorClassifiers(t *testing.T) {
	rateLimit := []string{
		"rate limit exceeded",
		"RATE_LIMIT",
		"monthly quota reached",
		"status 429",
		"Too Many Requests",
	}
	for _, msg := range rateLimit {
		if !isRateLimit(errors.New(msg)) {
			t.Errorf("isRateLimit(%q) = false", msg)
		}
	}
	connection := []string{
		"client not connected",
		"connection refused",
		"write: broken pipe",
		"unexpected EOF",
		"use of closed network connection",
	}
	for _, msg := range connection {
		if !isConnectionError(errors.New(msg)) {
			t.Errorf("isConnectionError(%q) = false", msg)
		}
	}
	other := errors.New("model tidak dikenal")
	if isRateLimit(other) || isConnectionError(other) {
		t.Errorf("%q tidak boleh dianggap rate limit maupun kesalahan koneksi", other)
	}
}

func TestShutdownOnColdPoolIsNoop(t *testing.T) {
	p := newTestPool("a", "b")
	p.Shutdown() // belum ada runtime yang dinyalakan — tidak boleh panic
	for _, acc := range p.accounts {
		if acc.client != nil {
			t.Fatalf("akun #%d masih punya client setelah Shutdown", acc.index)
		}
	}
}
