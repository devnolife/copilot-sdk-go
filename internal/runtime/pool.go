// Package runtime mengelola koneksi ke runtime Copilot CLI.
//
// Satu proses copilotd bisa memegang beberapa akun sekaligus (pool token).
// Tiap akun punya batas sesi paralel sendiri; akun yang kena rate limit
// dimasukkan cooldown agar permintaan berikutnya dialihkan ke akun lain.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// ErrAllBusy dikembalikan saat semua akun sedang cooldown atau penuh.
var ErrAllBusy = errors.New("semua akun Copilot sedang sibuk atau kena rate limit")

// cooldownAfterLimit adalah jeda sebelum akun yang kena rate limit dicoba lagi.
const cooldownAfterLimit = 5 * time.Minute

type account struct {
	token string
	// index dipakai hanya untuk pesan log agar token tidak pernah tercetak.
	index int

	mu          sync.Mutex
	client      *copilot.Client
	cooldownEnd time.Time

	slots chan struct{}
}

// Pool memilih akun yang paling longgar untuk tiap permintaan.
type Pool struct {
	cfg      config.Config
	accounts []*account
}

// NewPool menyiapkan pool tanpa menyalakan runtime apa pun (lazy start).
func NewPool(cfg config.Config) *Pool {
	tokens := cfg.Tokens()
	accounts := make([]*account, 0, len(tokens))
	for i, token := range tokens {
		accounts = append(accounts, &account{
			token: token,
			index: i + 1,
			slots: make(chan struct{}, cfg.MaxConcurrency),
		})
	}
	return &Pool{cfg: cfg, accounts: accounts}
}

// Size melaporkan jumlah akun dalam pool.
func (p *Pool) Size() int { return len(p.accounts) }

// Lease adalah hak pakai satu client sampai Release dipanggil.
type Lease struct {
	Client *copilot.Client

	pool *Pool
	acc  *account
	done bool
}

// Release mengembalikan slot ke pool. Aman dipanggil berkali-kali.
func (l *Lease) Release() {
	if l == nil || l.done {
		return
	}
	l.done = true
	<-l.acc.slots
}

// Failed menandai bahwa permintaan gagal. Kesalahan rate limit menaruh akun
// dalam cooldown; kesalahan koneksi membuang client agar dibangun ulang.
func (l *Lease) Failed(err error) {
	if l == nil || err == nil {
		return
	}
	if isRateLimit(err) {
		l.acc.markLimited()
		return
	}
	if isConnectionError(err) {
		l.acc.reset()
	}
}

// Acquire memilih akun yang tersedia lalu memastikan runtime-nya hidup.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	acc, err := p.pick(ctx)
	if err != nil {
		return nil, err
	}
	client, err := acc.ensure(ctx, p.cfg)
	if err != nil {
		<-acc.slots
		return nil, err
	}
	return &Lease{Client: client, pool: p, acc: acc}, nil
}

// pick mengambil slot dari akun pertama yang tidak cooldown dan tidak penuh.
// Kalau semua penuh, tunggu sampai ada yang longgar (atau context selesai).
func (p *Pool) pick(ctx context.Context) (*account, error) {
	now := time.Now()
	var fallback *account
	for _, acc := range p.accounts {
		if acc.inCooldown(now) {
			if fallback == nil {
				fallback = acc
			}
			continue
		}
		select {
		case acc.slots <- struct{}{}:
			return acc, nil
		default:
		}
	}
	// Semua akun sibuk: antre pada akun pertama yang tidak cooldown.
	for _, acc := range p.accounts {
		if acc.inCooldown(now) {
			continue
		}
		select {
		case acc.slots <- struct{}{}:
			return acc, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return nil, ErrAllBusy
		}
	}
	// Semuanya cooldown — coba yang paling awal pulih daripada menolak mentah.
	if fallback != nil {
		select {
		case fallback.slots <- struct{}{}:
			return fallback, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, ErrAllBusy
}

func (a *account) inCooldown(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return now.Before(a.cooldownEnd)
}

func (a *account) markLimited() {
	a.mu.Lock()
	a.cooldownEnd = time.Now().Add(cooldownAfterLimit)
	a.mu.Unlock()
	log.Printf("copilotd: akun #%d kena rate limit, cooldown %s", a.index, cooldownAfterLimit)
}

func (a *account) reset() {
	a.mu.Lock()
	client := a.client
	a.client = nil
	a.mu.Unlock()
	if client != nil {
		_ = client.Stop()
	}
}

// ensure menyalakan runtime bila belum ada. Start memakan beberapa detik,
// jadi client-nya dipakai ulang selama proses hidup.
func (a *account) ensure(ctx context.Context, cfg config.Config) (*copilot.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return a.client, nil
	}

	opts := &copilot.ClientOptions{
		LogLevel:         cfg.LogLevel,
		WorkingDirectory: cfg.WorkingDir,
		BaseDirectory:    cfg.BaseDir,
	}
	if a.token != "" {
		opts.GitHubToken = a.token
	} else {
		opts.UseLoggedInUser = copilot.Bool(true)
	}
	if conn := connection(cfg); conn != nil {
		opts.Connection = conn
	}

	client := copilot.NewClient(opts)
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("runtime Copilot gagal start: %w", err)
	}
	a.client = client
	return client, nil
}

// connection memilih transport ke runtime: server yang sudah jalan, binary
// tertentu, atau (nil) binary bawaan SDK.
//
// SDK mencocokkan tipe koneksi secara nilai, bukan pointer.
func connection(cfg config.Config) copilot.RuntimeConnection {
	switch {
	case cfg.CLIURL != "":
		return copilot.URIConnection{URL: cfg.CLIURL}
	case cfg.CLIPath != "":
		return copilot.StdioConnection{Path: cfg.CLIPath}
	default:
		return nil
	}
}

// Shutdown mematikan semua runtime yang sempat dinyalakan.
func (p *Pool) Shutdown() {
	for _, acc := range p.accounts {
		acc.reset()
	}
}

func isRateLimit(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"rate limit", "rate_limit", "quota", "429", "too many requests"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func isConnectionError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"not connected", "connection", "broken pipe", "eof", "closed"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
