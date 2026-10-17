// Package runtime mengelola koneksi ke runtime Copilot CLI.
//
// Satu proses copilotd bisa memegang beberapa akun sekaligus (pool token). Tiap akun punya batas sesi paralel
// sendiri. Akun yang kena rate limit atau kuotanya habis dimasukkan cooldown, dan akun yang sisa kuota premiumnya
// <= QuotaMinPercent ("hampir habis") hanya dipakai bila tidak ada akun lain, sehingga permintaan berikutnya
// dialihkan ke akun lain.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// ErrAllBusy dikembalikan saat semua akun sedang cooldown atau penuh.
var ErrAllBusy = errors.New("semua akun Copilot sedang sibuk atau kena rate limit")

// cooldownAfterLimit adalah jeda sebelum akun yang kena rate limit dicoba lagi.
const cooldownAfterLimit = 5 * time.Minute

// cooldownAfterQuota: kuota bulanan yang habis tidak pulih dalam hitungan menit.
const cooldownAfterQuota = time.Hour

// quotaTimeout membatasi satu pembacaan kuota di latar.
const quotaTimeout = 20 * time.Second

type account struct {
	token string
	// index dipakai hanya untuk pesan log agar token tidak pernah tercetak.
	index int

	mu          sync.Mutex
	client      *copilot.Client
	cooldownEnd time.Time
	limitKind   string // "rate_limit" | "quota" untuk cooldown terakhir

	// rpcAPI ditangkap saat runtime start: Client.Stop mengosongkan field RPC miliknya, jadi pembacaan kuota di
	// latar memakai salinan ini (galat bila runtime sudah mati, bukan data race).
	rpcAPI     *rpc.ServerRPC
	login      string
	quota      *QuotaInfo
	quotaAt    time.Time
	quotaErr   string
	refreshing bool

	slots chan struct{}
}

// Pool memilih akun yang sehat lebih dulu (urutan konfigurasi), lalu akun yang hampir habis kuotanya.
type Pool struct {
	cfg      config.Config
	accounts []*account
	// fetch membaca kuota satu akun; diganti di tes.
	fetch func(ctx context.Context, api *rpc.ServerRPC) (*QuotaInfo, error)
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
	return &Pool{cfg: cfg, accounts: accounts, fetch: fetchQuota}
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

// Release mengembalikan slot ke pool dan, bila sudah waktunya, membaca ulang kuota akun di latar. Aman dipanggil
// berkali-kali.
func (l *Lease) Release() {
	if l == nil || l.done {
		return
	}
	l.done = true
	<-l.acc.slots
	if l.pool != nil {
		l.pool.maybeRefresh(l.acc, false)
	}
}

// Failed menandai bahwa permintaan gagal. Kuota habis menaruh akun dalam cooldown panjang (dan kuotanya dibaca
// ulang), rate limit dalam cooldown pendek; kesalahan koneksi membuang client agar dibangun ulang.
func (l *Lease) Failed(err error) {
	if l == nil || err == nil {
		return
	}
	if isQuotaExhausted(err) {
		l.acc.markQuotaExhausted()
		if l.pool != nil {
			l.pool.maybeRefresh(l.acc, true)
		}
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
	p.maybeRefresh(acc, false)
	return &Lease{Client: client, pool: p, acc: acc}, nil
}

// HasAvailable: masih ada akun yang tidak cooldown (sehat atau hampir habis kuotanya).
func (p *Pool) HasAvailable() bool {
	now := time.Now()
	for _, acc := range p.accounts {
		if !acc.inCooldown(now) {
			return true
		}
	}
	return false
}

// pick mengambil slot dengan urutan: akun sehat (urutan konfigurasi), lalu akun yang kuotanya hampir habis (sisa
// terbesar dulu), lalu akun cooldown yang paling cepat pulih. Kalau akun di kelas pertama yang tersedia penuh
// semua, tunggu sampai ada yang longgar (atau context selesai): akun hampir habis tidak dipakai hanya karena akun
// sehat sedang sibuk.
func (p *Pool) pick(ctx context.Context) (*account, error) {
	now := time.Now()
	var fallback *account
	var healthy, low []*account
	for _, acc := range p.accounts {
		if acc.inCooldown(now) {
			// Kandidat cadangan: akun yang cooldown-nya paling cepat berakhir.
			if fallback == nil || acc.cooldownUntil().Before(fallback.cooldownUntil()) {
				fallback = acc
			}
			continue
		}
		if p.isLow(acc) {
			low = append(low, acc)
			continue
		}
		healthy = append(healthy, acc)
	}
	sort.SliceStable(low, func(i, j int) bool { return low[i].remaining() > low[j].remaining() })
	for _, group := range [][]*account{healthy, low} {
		if len(group) == 0 {
			continue
		}
		for _, acc := range group {
			select {
			case acc.slots <- struct{}{}:
				return acc, nil
			default:
			}
		}
		// Semua akun di kelas ini sibuk: antre pada akun pertamanya.
		select {
		case group[0].slots <- struct{}{}:
			return group[0], nil
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

func (a *account) cooldownUntil() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cooldownEnd
}

func (a *account) markLimited() {
	a.mu.Lock()
	a.cooldownEnd = time.Now().Add(cooldownAfterLimit)
	a.limitKind = "rate_limit"
	a.mu.Unlock()
	log.Printf("copilotd: akun #%d kena rate limit, cooldown %s", a.index, cooldownAfterLimit)
}

func (a *account) markQuotaExhausted() {
	a.mu.Lock()
	a.cooldownEnd = time.Now().Add(cooldownAfterQuota)
	a.limitKind = "quota"
	a.mu.Unlock()
	log.Printf("copilotd: akun #%d kuotanya habis, cooldown %s", a.index, cooldownAfterQuota)
}

// remaining: sisa persen kuota yang diketahui (101 = tidak diketahui/tak terbatas, dianggap paling longgar).
func (a *account) remaining() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quota == nil || a.quota.Unlimited {
		return 101
	}
	return a.quota.RemainingPercent
}

func (p *Pool) isLow(a *account) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return lowQuota(a.quota, p.cfg.QuotaMinPercent)
}

func (a *account) label() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.login != "" {
		return fmt.Sprintf("#%d (%s)", a.index, a.login)
	}
	return fmt.Sprintf("#%d", a.index)
}

// maybeRefresh membaca ulang kuota akun di latar bila runtime-nya hidup dan data terakhir sudah lewat
// QuotaCheckInterval (force = abaikan interval, mis. setelah galat kuota).
func (p *Pool) maybeRefresh(a *account, force bool) {
	if p.cfg.QuotaCheckInterval <= 0 || p.fetch == nil {
		return
	}
	a.mu.Lock()
	api := a.rpcAPI
	due := api != nil && !a.refreshing && (force || time.Since(a.quotaAt) >= p.cfg.QuotaCheckInterval)
	if due {
		a.refreshing = true
	}
	a.mu.Unlock()
	if !due {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout)
		defer cancel()
		q, err := p.fetch(ctx, api)
		p.applyQuota(a, q, err)
	}()
}

// applyQuota menyimpan hasil pembacaan kuota dan mencatat perubahan status hampir-habis.
func (p *Pool) applyQuota(a *account, q *QuotaInfo, err error) {
	a.mu.Lock()
	wasLow := lowQuota(a.quota, p.cfg.QuotaMinPercent)
	prevErr := a.quotaErr
	a.refreshing = false
	a.quotaAt = time.Now()
	if err != nil {
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		a.quotaErr = msg
	} else {
		a.quota, a.quotaErr = q, ""
	}
	isLow := lowQuota(a.quota, p.cfg.QuotaMinPercent)
	quota, quotaErr := a.quota, a.quotaErr
	a.mu.Unlock()

	switch {
	case err != nil && quotaErr != prevErr:
		log.Printf("copilotd: kuota akun %s tidak terbaca — %s", a.label(), quotaErr)
	case isLow && !wasLow:
		log.Printf("copilotd: akun %s sisa kuota %s %.1f%% <= %.1f%%: dipakai hanya bila akun lain tidak tersedia",
			a.label(), quota.Type, quota.RemainingPercent, p.cfg.QuotaMinPercent)
	case wasLow && !isLow && quota != nil:
		log.Printf("copilotd: akun %s kuota pulih (%s %.1f%%)", a.label(), quota.Type, quota.RemainingPercent)
	}
}

// AccountStatus adalah keadaan satu akun untuk /v1/status; token tidak pernah ikut.
type AccountStatus struct {
	Index          int        `json:"index"`
	Kind           string     `json:"kind"`
	Login          string     `json:"login,omitempty"`
	Started        bool       `json:"started"`
	InUse          int        `json:"in_use"`
	Capacity       int        `json:"capacity"`
	State          string     `json:"state"` // siap | hampir_habis | cooldown
	CooldownUntil  *time.Time `json:"cooldown_until,omitempty"`
	LimitKind      string     `json:"limit_kind,omitempty"`
	Quota          *QuotaInfo `json:"quota,omitempty"`
	QuotaCheckedAt *time.Time `json:"quota_checked_at,omitempty"`
	QuotaError     string     `json:"quota_error,omitempty"`
}

// Status melaporkan keadaan semua akun tanpa menyalakan runtime atau membaca kuota baru.
func (p *Pool) Status() []AccountStatus {
	now := time.Now()
	out := make([]AccountStatus, 0, len(p.accounts))
	for _, a := range p.accounts {
		a.mu.Lock()
		st := AccountStatus{
			Index: a.index, Kind: TokenKind(a.token), Login: a.login, Started: a.client != nil,
			InUse: len(a.slots), Capacity: cap(a.slots), State: "siap", QuotaError: a.quotaErr,
		}
		if a.quota != nil {
			q := *a.quota
			st.Quota = &q
		}
		if !a.quotaAt.IsZero() {
			at := a.quotaAt
			st.QuotaCheckedAt = &at
		}
		switch {
		case now.Before(a.cooldownEnd):
			until := a.cooldownEnd
			st.State, st.CooldownUntil, st.LimitKind = "cooldown", &until, a.limitKind
		case lowQuota(a.quota, p.cfg.QuotaMinPercent):
			st.State = "hampir_habis"
		}
		a.mu.Unlock()
		out = append(out, st)
	}
	return out
}

func (a *account) reset() {
	a.mu.Lock()
	client := a.client
	a.client = nil
	a.rpcAPI = nil
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
	a.rpcAPI = client.RPC
	if a.login == "" {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if st, err := client.GetAuthStatus(sctx); err == nil && st.Login != nil {
			a.login = *st.Login
		}
		cancel()
	}
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

// isQuotaExhausted: kuota (bulanan/premium) habis, bukan pembatasan laju sesaat.
func isQuotaExhausted(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"quota", "premium request", "exceeded your copilot", "entitlement"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
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
