// Package runtime mengelola koneksi ke runtime Copilot CLI.
//
// Satu proses copilotd memegang beberapa akun sekaligus (pool). Permintaan dibagi ke akun-akun yang setara menurut
// strategi pool (bawaan: proporsional terhadap sisa unit kuota) dengan smooth weighted round-robin. Urutan kelas:
// akun utama sehat → akun cadangan sehat (bila akun utama penuh atau tidak tersedia) → akun yang sisa kuota
// premiumnya <= QuotaMinPercent ("hampir habis") → akun cooldown yang paling cepat pulih. Akun nonaktif tidak
// pernah dipakai. Akun yang kena rate limit, kuotanya habis, atau tokennya ditolak masuk cooldown.
//
// Daftar akun dikelola lewat akun.json (paket akun); tambah, ubah, dan hapus berlaku tanpa restart.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// ErrAllBusy dikembalikan saat semua akun sedang cooldown atau penuh.
var ErrAllBusy = errors.New("semua akun Copilot sedang sibuk atau kena rate limit")

// ErrNoAccount: tidak ada akun yang boleh dipakai (semuanya nonaktif).
var ErrNoAccount = errors.New("tidak ada akun Copilot aktif")

// Galat pengelolaan akun; paket api memetakannya ke status HTTP.
var (
	ErrAccountInvalid   = errors.New("data akun tidak valid")
	ErrAccountDuplicate = errors.New("akun sudah ada di pool")
	ErrAccountNotFound  = errors.New("akun tidak ditemukan")
	ErrLastActive       = errors.New("minimal satu akun harus tetap aktif")
	ErrAccountRejected  = errors.New("token ditolak Copilot")
	ErrStoreUnavailable = errors.New("daftar akun tidak bisa diubah")
)

// cooldownAfterLimit adalah jeda sebelum akun yang kena rate limit dicoba lagi.
const cooldownAfterLimit = 5 * time.Minute

// cooldownAfterQuota: kuota bulanan yang habis tidak pulih dalam hitungan menit.
const cooldownAfterQuota = time.Hour

// cooldownAfterAuth: token dicabut/kedaluwarsa atau akun tanpa akses Copilot; dicoba lagi sesekali.
const cooldownAfterAuth = 15 * time.Minute

// cooldownAfterStart: runtime akun gagal dinyalakan.
const cooldownAfterStart = time.Minute

// modelUnavailableFor: lama sebuah akun dilewati untuk model yang ditolaknya.
const modelUnavailableFor = time.Hour

// quotaTimeout membatasi satu pembacaan kuota; identityTimeout satu pembacaan identitas akun.
const (
	quotaTimeout    = 20 * time.Second
	identityTimeout = 10 * time.Second
)

// Penggantian runtime. recycleMinAge: runtime yang lebih muda dari ini tidak diganti atas permintaan admin (datanya
// masih baru). recycleCheckEvery: jeda pemeriksaan umur runtime di latar. recycleRetryAfter: jeda sebelum akun yang
// runtime penggantinya gagal start dicoba lagi. recycleTimeout: batas satu start runtime pengganti di latar.
const (
	recycleMinAge     = time.Minute
	recycleCheckEvery = time.Minute
	recycleRetryAfter = 15 * time.Minute
	recycleTimeout    = 90 * time.Second
)

// busyWait: lama menunggu slot ketika semua akun yang pantas dipakai sedang penuh.
const busyWait = 30 * time.Second

// minWeight menjaga bobot proporsional tetap positif.
const minWeight = 1.0

// maxLabel adalah panjang maksimal label akun (karakter).
const maxLabel = 60

type account struct {
	// token dan id tidak berubah selama akun hidup; id aman ditampilkan, token tidak pernah.
	token string
	id    string

	// startMu menyerialkan start runtime tanpa menahan mu (start memakan beberapa detik).
	startMu sync.Mutex

	mu          sync.Mutex
	index       int
	source      string
	role        string
	label       string
	addedAt     time.Time
	addedBy     string
	removed     bool
	client      *copilot.Client
	cooldownEnd time.Time
	limitKind   string // rate_limit | quota | auth | start untuk cooldown terakhir
	// rpcAPI ditangkap saat runtime start: Client.Stop mengosongkan field RPC miliknya, jadi pembacaan kuota di
	// latar memakai salinan ini (galat bila runtime sudah mati, bukan data race).
	rpcAPI     *rpc.ServerRPC
	identity   akun.Identity
	quota      *QuotaInfo
	quotaAt    time.Time
	quotaErr   string
	refreshing bool
	lastUsed   time.Time
	lastErr    string
	lastErrAt  time.Time
	// noModel: model yang ditolak akun ini ("Model X is not available") sampai waktu tertentu; daftar model bisa
	// berbeda antar-akun (seat dari organisasi berbeda).
	noModel map[string]time.Time

	// Umur runtime. startedAt: kapan runtime yang terpasang menyala; ia membaca kuota dan daftar model akun saat itu
	// lalu menyimpannya selama hidup. modelsAt: kapan daftar model pertama dibaca dari runtime itu. draining: runtime
	// lama yang sudah diganti tetapi masih melayani permintaan berjalan; dimatikan begitu akun menganggur.
	// recycleRetryAt: paling cepat kapan penggantian otomatis dicoba lagi setelah gagal.
	startedAt      time.Time
	modelsAt       time.Time
	draining       []*copilot.Client
	recycleRetryAt time.Time

	// swrr adalah bobot berjalan smooth weighted round-robin; dijaga Pool.mu.
	swrr float64

	slots chan struct{}

	// stop mengganti cara mematikan runtime di tes (nil = Client.Stop).
	stop func(*copilot.Client)
}

// Pool membagi permintaan ke akun-akun Copilot.
type Pool struct {
	cfg config.Config

	mu        sync.RWMutex
	accounts  []*account
	strategy  string
	storePath string // "" = perubahan daftar akun hanya di memori
	managed   bool   // daftar akun dibaca dari / ditulis ke akun.json
	// storeErr: akun.json ada tetapi tidak terbaca. Perubahan ditolak sampai berkas diperbaiki, supaya akun yang
	// hanya ada di berkas itu tidak tertimpa daftar dari .env.
	storeErr string

	wakeMu sync.Mutex
	wake   chan struct{} // ditutup lalu diganti setiap ada slot yang lepas

	usage *usageBook

	// Kait yang diganti di tes.
	fetch    func(ctx context.Context, api *rpc.ServerRPC, token string) (*QuotaInfo, error)
	identify func(ctx context.Context, api *rpc.ServerRPC) (*akun.Identity, error)
	probe    func(ctx context.Context, cfg config.Config, token string) (AccountProbe, error)
	spawn    func(ctx context.Context, cfg config.Config, token string) (*copilot.Client, *rpc.ServerRPC, error)
	now      func() time.Time
}

// NewPool menyiapkan pool tanpa menyalakan runtime apa pun (lazy start). Daftar akun dibaca dari akun.json bila
// ada di cfg.StateDir, selain itu dari TURNITIN_COPILOT_GITHUB_TOKEN(S).
func NewPool(cfg config.Config) *Pool {
	p := &Pool{
		cfg: cfg, strategy: cfg.PoolStrategy, storePath: akun.PathIn(cfg.StateDir), wake: make(chan struct{}),
		fetch: fetchQuota, identify: readIdentity, probe: ProbeAccount, spawn: startRuntime, now: time.Now,
	}
	var records []akun.Record
	if p.storePath != "" {
		f, exists, err := akun.Load(p.storePath)
		switch {
		case err != nil:
			p.storeErr = err.Error()
			log.Printf("copilotd: %v; memakai akun dari .env, perubahan akun ditolak sampai berkas diperbaiki", err)
		case exists && len(f.Accounts) == 0:
			log.Printf("copilotd: %s tidak berisi akun; memakai akun dari .env", p.storePath)
		case exists:
			records, p.managed, p.strategy = f.Accounts, true, f.Strategy
		}
	}
	if !akun.ValidStrategy(p.strategy) {
		p.strategy = akun.StrategyProporsional
	}
	if p.managed {
		if len(cfg.GitHubTokens) > 0 || cfg.GitHubToken != "" {
			log.Printf("copilotd: daftar akun dibaca dari %s; TURNITIN_COPILOT_GITHUB_TOKEN(S) di .env diabaikan", p.storePath)
		}
	} else {
		records = akun.FromTokens(cfg.Tokens(), p.strategy, time.Now()).Accounts
	}
	for _, r := range records {
		p.accounts = append(p.accounts, p.newAccount(r))
	}
	p.reindexLocked()
	p.usage = newUsageBook(usagePathIn(cfg.StateDir), cfg.TimeZone)
	return p
}

func (p *Pool) newAccount(r akun.Record) *account {
	role := r.Role
	if !akun.ValidRole(role) {
		role = akun.RoleUtama
	}
	return &account{
		token: r.Token, id: akun.Fingerprint(r.Token), source: r.Source, role: role, label: r.Label,
		addedAt: r.AddedAt, addedBy: r.AddedBy, identity: r.Identity,
		slots: make(chan struct{}, max(1, p.cfg.MaxConcurrency)),
	}
}

// reindexLocked memberi nomor urut 1..n untuk tampilan dan log (pemanggil memegang p.mu atau belum berbagi pool).
func (p *Pool) reindexLocked() {
	for i, a := range p.accounts {
		a.mu.Lock()
		a.index = i + 1
		a.mu.Unlock()
	}
}

// Size melaporkan jumlah akun dalam pool.
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// Strategy melaporkan strategi pembagian yang berlaku.
func (p *Pool) Strategy() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategy
}

// Source melaporkan asal daftar akun: berkas akun.json atau .env.
func (p *Pool) Source() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.managed {
		return p.storePath
	}
	return ".env"
}

func (p *Pool) list() []*account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.accounts)
}

func (p *Pool) findLocked(id string) *account {
	for _, a := range p.accounts {
		if a.id == id {
			return a
		}
	}
	return nil
}

func (p *Pool) waitChan() <-chan struct{} {
	p.wakeMu.Lock()
	defer p.wakeMu.Unlock()
	return p.wake
}

// notify membangunkan semua pick yang sedang menunggu slot.
func (p *Pool) notify() {
	p.wakeMu.Lock()
	close(p.wake)
	p.wake = make(chan struct{})
	p.wakeMu.Unlock()
}

// Lease adalah hak pakai satu client sampai Release dipanggil.
type Lease struct {
	Client *copilot.Client

	pool     *Pool
	acc      *account
	started  time.Time
	model    string
	done     bool
	recorded bool
}

// AccountID adalah id akun yang melayani lease ini.
func (l *Lease) AccountID() string {
	if l == nil || l.acc == nil {
		return ""
	}
	return l.acc.id
}

// Release mengembalikan slot ke pool dan, bila sudah waktunya, membaca ulang kuota akun di latar. Aman dipanggil
// berkali-kali.
func (l *Lease) Release() {
	if l == nil || l.done {
		return
	}
	l.done = true
	if l.pool == nil {
		<-l.acc.slots
		return
	}
	l.pool.releaseSlot(l.acc)
	l.pool.maybeRefresh(l.acc, false)
}

// Failed menandai bahwa permintaan gagal. Kuota habis menaruh akun dalam cooldown panjang (dan kuotanya dibaca
// ulang), rate limit dalam cooldown pendek, token ditolak dalam cooldown menengah; kesalahan koneksi membuang
// client lease ini agar dibangun ulang.
func (l *Lease) Failed(err error) {
	if l == nil || err == nil {
		return
	}
	switch {
	case isQuotaExhausted(err):
		l.acc.markCooldown("quota", cooldownAfterQuota)
		if l.pool != nil {
			l.pool.maybeRefresh(l.acc, true)
		}
	case isRateLimit(err):
		l.acc.markCooldown("rate_limit", cooldownAfterLimit)
	case isAuthError(err):
		l.acc.markCooldown("auth", cooldownAfterAuth)
	case isConnectionError(err):
		l.acc.drop(l.Client)
	}
}

// ModelUnavailable menandai bahwa akun lease ini menolak model-nya; akun itu dilewati untuk model tersebut selama
// modelUnavailableFor. Mengembalikan true bila masih ada akun lain yang bisa dicoba.
func (l *Lease) ModelUnavailable() bool {
	if l == nil || l.acc == nil || l.model == "" {
		return false
	}
	a := l.acc
	a.mu.Lock()
	if a.noModel == nil {
		a.noModel = map[string]time.Time{}
	}
	a.noModel[l.model] = time.Now().Add(modelUnavailableFor)
	a.mu.Unlock()
	log.Printf("copilotd: akun %s tidak menyediakan model %s; dilewati untuk model ini selama %s", a.logName(), l.model, modelUnavailableFor)
	return l.pool != nil && l.pool.Size() > 1
}

// Done mencatat hasil satu permintaan generasi untuk statistik pemakaian per akun (sekali per lease).
func (l *Lease) Done(err error, calls []UsageCall) {
	if l == nil || l.recorded || l.pool == nil || l.acc == nil {
		return
	}
	l.recorded = true
	l.pool.record(l.acc, err, calls, l.pool.now().Sub(l.started))
}

// Acquire memilih akun yang tersedia lalu memastikan runtime-nya hidup. Bila runtime gagal dinyalakan dan masih
// ada akun lain, galatnya ditandai layak diulang di akun lain.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	return p.AcquireModel(ctx, "")
}

// AcquireModel seperti Acquire, tetapi melewati akun yang baru saja menolak model itu selama masih ada akun lain.
func (p *Pool) AcquireModel(ctx context.Context, model string) (*Lease, error) {
	acc, err := p.pick(ctx, model)
	if err != nil {
		return nil, err
	}
	client, started, err := p.ensure(ctx, acc)
	if err != nil {
		return nil, p.failStart(ctx, acc, err)
	}
	if started {
		p.afterStart(acc)
	} else {
		p.maybeRefresh(acc, false)
	}
	return &Lease{Client: client, pool: p, acc: acc, started: p.now(), model: model}, nil
}

// afterStart membaca identitas dan kuota akun di latar lewat runtime yang baru menyala.
func (p *Pool) afterStart(a *account) {
	p.refreshIdentity(a)
	p.maybeRefresh(a, true)
}

// failStart melepas slot akun yang runtime-nya gagal start. Bila penyebabnya context pemanggil (dibatalkan atau
// lewat tenggat), akun tidak disalahkan: tanpa cooldown dan tanpa pengulangan ke akun lain, supaya satu klien yang
// putus tidak menaruh semua akun dalam cooldown satu per satu.
func (p *Pool) failStart(ctx context.Context, acc *account, err error) error {
	p.releaseSlot(acc)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, errAccountRemoved) {
		return err
	}
	acc.noteError(err.Error(), p.now())
	acc.markCooldown("start", cooldownAfterStart)
	if p.Size() > 1 {
		return fmt.Errorf("%w: %v", errRetryable, err)
	}
	return err
}

func (p *Pool) releaseSlot(a *account) {
	<-a.slots
	a.mu.Lock()
	idle := len(a.slots) == 0
	stop := a.removed && idle
	var old []*copilot.Client
	if idle && !stop {
		// Permintaan terakhir yang mungkin masih memakai runtime lama sudah selesai.
		old, a.draining = a.draining, nil
	}
	a.mu.Unlock()
	if stop {
		go a.reset()
	}
	if len(old) > 0 {
		go a.halt(old...)
	}
	p.notify()
}

// HasAvailable: masih ada akun yang boleh dipakai dan tidak cooldown (sehat atau hampir habis kuotanya).
func (p *Pool) HasAvailable() bool {
	now := p.now()
	for _, a := range p.list() {
		a.mu.Lock()
		ok := a.role != akun.RoleNonaktif && !now.Before(a.cooldownEnd)
		a.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
}

// pick mengambil satu slot menurut kelas dan strategi (lihat dokumentasi paket). Kalau akun yang pantas dipakai
// sedang penuh semua, tunggu slot lepas (maks busyWait) daripada beralih ke akun yang kuotanya hampir habis.
func (p *Pool) pick(ctx context.Context, model string) (*account, error) {
	timer := time.NewTimer(busyWait)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wait := p.waitChan()
		acc, err := p.tryPick(model)
		if acc != nil || err != nil {
			return acc, err
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrAllBusy
		}
	}
}

// tryPick mengambil slot tanpa menunggu; (nil, nil) berarti ada kandidat tetapi semuanya penuh. Akun yang baru
// menolak model diperlakukan seperti akun cooldown untuk model itu: hanya dipakai bila tidak ada akun lain.
func (p *Pool) tryPick(model string) (*account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var main, reserve, low []*account
	var fallback *account
	var fallbackUntil time.Time
	for _, a := range p.accounts {
		a.mu.Lock()
		role, until := a.role, a.cooldownEnd
		if noUntil, ok := a.noModel[model]; ok && model != "" && now.Before(noUntil) && noUntil.After(until) {
			until = noUntil
		}
		isLow := lowQuota(a.quota, p.cfg.QuotaMinPercent)
		a.mu.Unlock()
		switch {
		case role == akun.RoleNonaktif:
		case now.Before(until):
			if fallback == nil || until.Before(fallbackUntil) {
				fallback, fallbackUntil = a, until
			}
		case isLow:
			low = append(low, a)
		case role == akun.RoleCadangan:
			reserve = append(reserve, a)
		default:
			main = append(main, a)
		}
	}
	for _, group := range [][]*account{main, reserve} {
		if acc := p.chooseLocked(group); acc != nil {
			return acc, nil
		}
	}
	if len(main)+len(reserve) > 0 {
		return nil, nil
	}
	sort.SliceStable(low, func(i, j int) bool { return low[i].remaining() > low[j].remaining() })
	for _, a := range low {
		if take(a) {
			return a, nil
		}
	}
	if len(low) > 0 {
		return nil, nil
	}
	// Semuanya cooldown: coba yang paling awal pulih daripada menolak mentah.
	if fallback != nil {
		if take(fallback) {
			return fallback, nil
		}
		return nil, nil
	}
	return nil, ErrNoAccount
}

func take(a *account) bool {
	select {
	case a.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// chooseLocked memilih satu akun berslot longgar di kelompok menurut strategi pool lalu mengambil slotnya.
func (p *Pool) chooseLocked(group []*account) *account {
	if p.strategy == akun.StrategyPrioritas {
		for _, a := range group {
			if take(a) {
				return a
			}
		}
		return nil
	}
	free := make([]*account, 0, len(group))
	for _, a := range group {
		if len(a.slots) < cap(a.slots) {
			free = append(free, a)
		}
	}
	if len(free) == 0 {
		return nil
	}
	weights := p.weights(free)
	total := 0.0
	var best *account
	for i, a := range free {
		a.swrr += weights[i]
		total += weights[i]
		if best == nil || a.swrr > best.swrr {
			best = a
		}
	}
	best.swrr -= total
	if take(best) {
		return best
	}
	for _, a := range free {
		if take(a) {
			return a
		}
	}
	return nil
}

// weights memberi bobot pembagian. Proporsional: sisa unit kuota (sisa persen bila entitlement tidak diketahui);
// kuota tak diketahui = rata-rata bobot yang diketahui; tak terbatas = bobot terbesar. Bergiliran: sama rata.
func (p *Pool) weights(group []*account) []float64 {
	w := make([]float64, len(group))
	if p.strategy == akun.StrategyBergiliran {
		for i := range w {
			w[i] = 1
		}
		return w
	}
	var known []float64
	var unknown, unlimited []int
	for i, a := range group {
		a.mu.Lock()
		q := a.quota
		a.mu.Unlock()
		switch {
		case q == nil:
			unknown = append(unknown, i)
		case q.Unlimited:
			unlimited = append(unlimited, i)
		default:
			v := q.RemainingPercent
			if units, ok := q.remainingUnits(); ok {
				v = units
			}
			v = max(v, minWeight)
			w[i] = v
			known = append(known, v)
		}
	}
	fill, top := 1.0, 1.0
	if len(known) > 0 {
		sum := 0.0
		for _, v := range known {
			sum += v
		}
		fill, top = sum/float64(len(known)), slices.Max(known)
	}
	for _, i := range unknown {
		w[i] = fill
	}
	for _, i := range unlimited {
		w[i] = top
	}
	return w
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

func (a *account) markCooldown(kind string, d time.Duration) {
	a.mu.Lock()
	a.cooldownEnd = time.Now().Add(d)
	a.limitKind = kind
	a.mu.Unlock()
	log.Printf("copilotd: akun %s %s, cooldown %s", a.logName(), cooldownReason(kind), d)
}

// markLimited dan markQuotaExhausted dipertahankan untuk pemanggil lama dan tes.
func (a *account) markLimited()        { a.markCooldown("rate_limit", cooldownAfterLimit) }
func (a *account) markQuotaExhausted() { a.markCooldown("quota", cooldownAfterQuota) }

func cooldownReason(kind string) string {
	switch kind {
	case "quota":
		return "kuotanya habis"
	case "auth":
		return "ditolak (token/akses Copilot)"
	case "start":
		return "runtime-nya gagal dinyalakan"
	default:
		return "kena rate limit"
	}
}

func (a *account) noteError(msg string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	msg = redact(msg, a.token)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	a.lastErr, a.lastErrAt = msg, at
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

func (a *account) logName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.identity.Login != "" {
		return fmt.Sprintf("#%d (%s)", a.index, a.identity.Login)
	}
	return fmt.Sprintf("#%d", a.index)
}

func (a *account) api() *rpc.ServerRPC {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rpcAPI
}

// record memperbarui waktu pakai/galat terakhir akun dan catatan pemakaian hariannya.
func (p *Pool) record(a *account, err error, calls []UsageCall, elapsed time.Duration) {
	now := p.now()
	c := UsageCounts{Requests: 1, DurationMs: elapsed.Milliseconds()}
	switch {
	case err == nil:
		c.OK = 1
	case isQuotaExhausted(err):
		c.Failed, c.QuotaExhausted = 1, 1
	case isRateLimit(err):
		c.Failed, c.RateLimited = 1, 1
	default:
		c.Failed = 1
	}
	for _, u := range calls {
		c.ModelCalls++
		c.InputTokens += u.InputTokens
		c.OutputTokens += u.OutputTokens
		c.CacheReadTokens += u.CacheReadTokens
		c.CacheWriteTokens += u.CacheWriteTokens
		c.Cost += u.Cost
		c.NanoAiu += u.NanoAiu
	}
	a.mu.Lock()
	a.lastUsed = now
	login, label := a.identity.Login, a.label
	a.mu.Unlock()
	if err != nil {
		a.noteError(err.Error(), now)
	}
	p.usage.record(a.id, login, label, now, c)
}

// maybeRefresh membaca ulang kuota akun lewat runtime-nya sendiri di latar bila runtime hidup dan data terakhir
// sudah lewat QuotaCheckInterval (force = abaikan interval, mis. setelah galat kuota atau runtime baru start).
func (p *Pool) maybeRefresh(a *account, force bool) {
	if api := a.api(); api != nil {
		p.refreshQuota(a, api, "", force)
	}
}

// refreshQuota membaca kuota a lewat api di latar. token kosong = akun milik runtime api; terisi = token a dibaca
// lewat runtime akun lain.
func (p *Pool) refreshQuota(a *account, api *rpc.ServerRPC, token string, force bool) {
	if p.cfg.QuotaCheckInterval <= 0 || p.fetch == nil || api == nil {
		return
	}
	a.mu.Lock()
	due := !a.refreshing && (force || p.now().Sub(a.quotaAt) >= p.cfg.QuotaCheckInterval)
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
		q, err := p.fetch(ctx, api, token)
		p.applyQuota(a, q, err)
	}()
}

// refreshIdle membaca ulang kuota akun yang datanya sudah usang tanpa menyalakan runtime baru: lewat runtime akun
// itu sendiri, atau (akun bertoken) lewat runtime akun lain yang sedang hidup.
func (p *Pool) refreshIdle() {
	accs := p.list()
	var reader *rpc.ServerRPC
	for _, a := range accs {
		if api := a.api(); api != nil {
			reader = api
			break
		}
	}
	for _, a := range accs {
		if api := a.api(); api != nil {
			p.refreshQuota(a, api, "", false)
		} else if a.token != "" && reader != nil {
			p.refreshQuota(a, reader, a.token, false)
		}
	}
}

// applyQuota menyimpan hasil pembacaan kuota dan mencatat perubahan status hampir-habis.
func (p *Pool) applyQuota(a *account, q *QuotaInfo, err error) {
	now := p.now()
	a.mu.Lock()
	wasLow := lowQuota(a.quota, p.cfg.QuotaMinPercent)
	prevErr := a.quotaErr
	a.refreshing = false
	a.quotaAt = now
	if err != nil {
		msg := redact(err.Error(), a.token)
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

	if err == nil && q != nil {
		p.usage.noteQuota(a.id, now, q)
	}
	switch {
	case err != nil && quotaErr != prevErr:
		log.Printf("copilotd: kuota akun %s tidak terbaca — %s", a.logName(), quotaErr)
	case isLow && !wasLow:
		log.Printf("copilotd: akun %s sisa kuota %s %.1f%% <= %.1f%%: dipakai hanya bila akun lain tidak tersedia",
			a.logName(), quota.Type, quota.RemainingPercent, p.cfg.QuotaMinPercent)
	case wasLow && !isLow && quota != nil:
		log.Printf("copilotd: akun %s kuota pulih (%s %.1f%%)", a.logName(), quota.Type, quota.RemainingPercent)
	}
}

// refreshIdentity membaca login, plan, dan tanggal reset akun di latar lewat runtime-nya sendiri.
func (p *Pool) refreshIdentity(a *account) {
	api := a.api()
	if api == nil || p.identify == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), identityTimeout)
		defer cancel()
		id, err := p.identify(ctx, api)
		if err != nil {
			log.Printf("copilotd: identitas akun %s tidak terbaca — %s", a.logName(), redact(err.Error(), a.token))
			return
		}
		p.applyIdentity(a, id)
	}()
}

// applyIdentity menyimpan identitas akun; akun.json ditulis ulang (tanpa cadangan) hanya bila isinya berubah.
func (p *Pool) applyIdentity(a *account, id *akun.Identity) {
	if id == nil {
		return
	}
	a.mu.Lock()
	merged := *id
	if merged.Login == "" {
		merged.Login = a.identity.Login
	}
	changed := !sameIdentity(a.identity, merged)
	a.identity = merged
	label := a.label
	a.mu.Unlock()
	p.usage.setNames(a.id, merged.Login, label)
	if !changed {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.managed {
		if err := p.saveLocked(false); err != nil {
			log.Printf("copilotd: %v", err)
		}
	}
}

func sameIdentity(a, b akun.Identity) bool {
	sameTime := (a.ResetAt == nil && b.ResetAt == nil) || (a.ResetAt != nil && b.ResetAt != nil && a.ResetAt.Equal(*b.ResetAt))
	sameBilling := (a.TokenBasedBilling == nil && b.TokenBasedBilling == nil) ||
		(a.TokenBasedBilling != nil && b.TokenBasedBilling != nil && *a.TokenBasedBilling == *b.TokenBasedBilling)
	return a.Login == b.Login && a.Plan == b.Plan && sameTime && sameBilling
}

// Refresh membaca ulang identitas dan kuota sekarang juga (id kosong = semua akun). Runtime akun yang boleh dipakai
// dinyalakan bila perlu, dan runtime yang sudah lama hidup diganti dulu karena hanya bisa melaporkan kuota yang
// dibacanya saat menyala; akun nonaktif bertoken hanya dibaca kuotanya lewat runtime akun lain.
func (p *Pool) Refresh(ctx context.Context, id string) error {
	accs := p.list()
	if id != "" {
		p.mu.RLock()
		a := p.findLocked(id)
		p.mu.RUnlock()
		if a == nil {
			return ErrAccountNotFound
		}
		accs = []*account{a}
	}
	for _, a := range accs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.mu.Lock()
		disabled, removed := a.role == akun.RoleNonaktif, a.removed
		a.mu.Unlock()
		if removed {
			continue
		}
		if disabled && id == "" {
			if a.token != "" {
				if reader := p.anyRuntime(); reader != nil {
					p.refreshNow(ctx, a, reader, a.token)
				}
			}
			continue
		}
		if _, err := p.recycle(ctx, a, recycleMinAge); err != nil {
			p.applyQuota(a, nil, fmt.Errorf("kuota belum diperbarui, runtime baru gagal start: %w", err))
			continue
		}
		if _, _, err := p.ensure(ctx, a); err != nil {
			p.applyQuota(a, nil, err)
			continue
		}
		api := a.api()
		if p.identify != nil {
			ictx, cancel := context.WithTimeout(ctx, identityTimeout)
			ident, err := p.identify(ictx, api)
			cancel()
			if err == nil {
				p.applyIdentity(a, ident)
			}
		}
		p.refreshNow(ctx, a, api, "")
	}
	return nil
}

func (p *Pool) refreshNow(ctx context.Context, a *account, api *rpc.ServerRPC, token string) {
	if p.fetch == nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	q, err := p.fetch(qctx, api, token)
	p.applyQuota(a, q, err)
}

func (p *Pool) anyRuntime() *rpc.ServerRPC {
	for _, a := range p.list() {
		if api := a.api(); api != nil {
			return api
		}
	}
	return nil
}

// AccountStatus adalah keadaan satu akun untuk /v1/status dan /v1/accounts; token tidak pernah ikut.
type AccountStatus struct {
	ID                string     `json:"id"`
	Index             int        `json:"index"`
	Kind              string     `json:"kind"`
	Source            string     `json:"source"`
	Role              string     `json:"role"`
	Label             string     `json:"label,omitempty"`
	Login             string     `json:"login,omitempty"`
	Plan              string     `json:"plan,omitempty"`
	TokenBasedBilling *bool      `json:"token_based_billing,omitempty"`
	ResetAt           *time.Time `json:"reset_at,omitempty"`
	Started           bool       `json:"started"`
	// RuntimeStartedAt: kapan runtime akun menyala; kuota yang dilaporkannya tidak lebih baru dari saat itu.
	RuntimeStartedAt *time.Time `json:"runtime_started_at,omitempty"`
	InUse            int        `json:"in_use"`
	Capacity         int        `json:"capacity"`
	State            string     `json:"state"` // siap | sibuk | hampir_habis | cooldown | nonaktif
	CooldownUntil    *time.Time `json:"cooldown_until,omitempty"`
	LimitKind        string     `json:"limit_kind,omitempty"`
	Quota            *QuotaInfo `json:"quota,omitempty"`
	QuotaCheckedAt   *time.Time `json:"quota_checked_at,omitempty"`
	QuotaError       string     `json:"quota_error,omitempty"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	LastErrorAt      *time.Time `json:"last_error_at,omitempty"`
	// ModelsUnavailable: model yang baru ditolak akun ini, sehingga permintaan model itu diarahkan ke akun lain.
	ModelsUnavailable []string   `json:"models_unavailable,omitempty"`
	AddedAt           *time.Time `json:"added_at,omitempty"`
	AddedBy           string     `json:"added_by,omitempty"`
}

// Status melaporkan keadaan semua akun tanpa menyalakan runtime atau membaca kuota baru.
func (p *Pool) Status() []AccountStatus {
	now := p.now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AccountStatus, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, p.statusOf(a, now))
	}
	return out
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func (p *Pool) statusOf(a *account, now time.Time) AccountStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := AccountStatus{
		ID: a.id, Index: a.index, Kind: TokenKind(a.token), Source: a.source, Role: a.role, Label: a.label,
		Login: a.identity.Login, Plan: a.identity.Plan, Started: a.client != nil, InUse: len(a.slots),
		Capacity: cap(a.slots), State: "siap", QuotaError: a.quotaErr, LastError: a.lastErr, AddedBy: a.addedBy,
		QuotaCheckedAt: timePtr(a.quotaAt), LastUsedAt: timePtr(a.lastUsed), LastErrorAt: timePtr(a.lastErrAt),
		AddedAt: timePtr(a.addedAt), RuntimeStartedAt: timePtr(a.startedAt),
	}
	if a.identity.ResetAt != nil {
		st.ResetAt = timePtr(*a.identity.ResetAt)
	}
	if a.identity.TokenBasedBilling != nil {
		v := *a.identity.TokenBasedBilling
		st.TokenBasedBilling = &v
	}
	if a.quota != nil {
		q := *a.quota
		st.Quota = &q
	}
	for model, until := range a.noModel {
		if now.Before(until) {
			st.ModelsUnavailable = append(st.ModelsUnavailable, model)
		}
	}
	sort.Strings(st.ModelsUnavailable)
	switch {
	case a.role == akun.RoleNonaktif:
		st.State = "nonaktif"
	case now.Before(a.cooldownEnd):
		st.State, st.CooldownUntil, st.LimitKind = "cooldown", timePtr(a.cooldownEnd), a.limitKind
	case lowQuota(a.quota, p.cfg.QuotaMinPercent):
		st.State = "hampir_habis"
	case st.InUse >= st.Capacity:
		st.State = "sibuk"
	}
	return st
}

// AddInput adalah permintaan menambah akun. Token hanya dipakai untuk probe dan disimpan di akun.json.
type AddInput struct {
	Token  string
	Role   string
	Label  string
	Actor  string
	Source string
}

// UpdateInput mengubah peran dan/atau label akun (nil = tidak diubah).
type UpdateInput struct {
	Role  *string
	Label *string
	Actor string
}

// Add memeriksa token di runtime sementara lalu menambahkannya ke pool; langsung dipakai tanpa restart.
func (p *Pool) Add(ctx context.Context, in AddInput) (AccountStatus, error) {
	token := strings.TrimSpace(in.Token)
	if err := validateNewToken(token); err != nil {
		return AccountStatus{}, err
	}
	role := in.Role
	if role == "" {
		role = akun.RoleUtama
	}
	if !akun.ValidRole(role) {
		return AccountStatus{}, fmt.Errorf("%w: peran %q tidak dikenal", ErrAccountInvalid, role)
	}
	label, err := cleanLabel(in.Label)
	if err != nil {
		return AccountStatus{}, err
	}
	source := in.Source
	if source == "" {
		source = akun.SourceAdmin
	}
	actor := cleanActor(in.Actor)
	id := akun.Fingerprint(token)
	p.mu.RLock()
	exists := p.findLocked(id) != nil
	p.mu.RUnlock()
	if exists {
		return AccountStatus{}, fmt.Errorf("%w: token ini sudah ada di pool", ErrAccountDuplicate)
	}

	probe, err := p.probe(ctx, p.cfg, token)
	if err != nil {
		return AccountStatus{}, fmt.Errorf("%w: %s", ErrAccountRejected, redact(err.Error(), token))
	}

	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.findLocked(id) != nil {
		return AccountStatus{}, fmt.Errorf("%w: token ini sudah ada di pool", ErrAccountDuplicate)
	}
	if probe.Login != "" {
		for _, other := range p.accounts {
			other.mu.Lock()
			same := strings.EqualFold(other.identity.Login, probe.Login)
			other.mu.Unlock()
			if same {
				return AccountStatus{}, fmt.Errorf("%w: akun %s sudah ada di pool; token kedua untuk akun yang sama tidak menambah kuota",
					ErrAccountDuplicate, probe.Login)
			}
		}
	}
	acc := p.newAccount(akun.Record{
		ID: id, Token: token, Source: source, Role: role, Label: label, AddedAt: now.UTC(), AddedBy: actor,
		Identity: probe.identity(now),
	})
	if probe.Quota != nil {
		q := *probe.Quota
		acc.quota, acc.quotaAt = &q, now
	}
	p.accounts = append(p.accounts, acc)
	p.reindexLocked()
	if err := p.saveLocked(true); err != nil {
		p.accounts = p.accounts[:len(p.accounts)-1]
		p.reindexLocked()
		return AccountStatus{}, err
	}
	if probe.Quota != nil {
		p.usage.noteQuota(id, now, probe.Quota)
	}
	p.usage.restore(id)
	p.usage.setNames(id, probe.Login, label)
	log.Printf("copilotd: akun %s ditambahkan sebagai %s (id %s, %s) oleh %s", acc.logName(), role, id, TokenKind(token), actorOrDash(actor))
	p.notify()
	return p.statusOf(acc, now), nil
}

// Update mengubah peran dan/atau label akun.
func (p *Pool) Update(id string, in UpdateInput) (AccountStatus, error) {
	var label string
	if in.Label != nil {
		clean, err := cleanLabel(*in.Label)
		if err != nil {
			return AccountStatus{}, err
		}
		label = clean
	}
	if in.Role != nil && !akun.ValidRole(*in.Role) {
		return AccountStatus{}, fmt.Errorf("%w: peran %q tidak dikenal", ErrAccountInvalid, *in.Role)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	acc := p.findLocked(id)
	if acc == nil {
		return AccountStatus{}, ErrAccountNotFound
	}
	acc.mu.Lock()
	oldRole, oldLabel := acc.role, acc.label
	acc.mu.Unlock()
	newRole, newLabel := oldRole, oldLabel
	if in.Role != nil {
		newRole = *in.Role
	}
	if in.Label != nil {
		newLabel = label
	}
	if newRole == akun.RoleNonaktif && oldRole != akun.RoleNonaktif && p.activeCountLocked() <= 1 {
		return AccountStatus{}, ErrLastActive
	}
	acc.mu.Lock()
	acc.role, acc.label = newRole, newLabel
	acc.mu.Unlock()
	if err := p.saveLocked(true); err != nil {
		acc.mu.Lock()
		acc.role, acc.label = oldRole, oldLabel
		acc.mu.Unlock()
		return AccountStatus{}, err
	}
	for _, a := range p.accounts {
		a.swrr = 0
	}
	p.usage.setNames(id, "", newLabel)
	log.Printf("copilotd: akun %s diubah (peran %s → %s) oleh %s", acc.logName(), oldRole, newRole, actorOrDash(cleanActor(in.Actor)))
	p.notify()
	return p.statusOf(acc, p.now()), nil
}

// Remove mengeluarkan akun dari pool. Permintaan yang sedang berjalan di akun itu dibiarkan selesai; runtime-nya
// dimatikan setelah slot terakhir lepas. Riwayat pemakaiannya tetap disimpan.
func (p *Pool) Remove(id, actor string) error {
	p.mu.Lock()
	acc := p.findLocked(id)
	if acc == nil {
		p.mu.Unlock()
		return ErrAccountNotFound
	}
	acc.mu.Lock()
	role := acc.role
	acc.mu.Unlock()
	if role != akun.RoleNonaktif && p.activeCountLocked() <= 1 {
		p.mu.Unlock()
		return ErrLastActive
	}
	idx := slices.Index(p.accounts, acc)
	before := slices.Clone(p.accounts)
	p.accounts = slices.Delete(p.accounts, idx, idx+1)
	if err := p.saveLocked(true); err != nil {
		p.accounts = before
		p.mu.Unlock()
		return err
	}
	name := acc.logName()
	p.reindexLocked()
	acc.mu.Lock()
	acc.removed = true
	idle := len(acc.slots) == 0
	login, label := acc.identity.Login, acc.label
	acc.mu.Unlock()
	p.mu.Unlock()

	p.usage.markRemoved(id, login, label, p.now())
	if idle {
		acc.reset()
	}
	log.Printf("copilotd: akun %s dihapus dari pool (id %s) oleh %s", name, id, actorOrDash(cleanActor(actor)))
	p.notify()
	return nil
}

// SetStrategy mengganti strategi pembagian.
func (p *Pool) SetStrategy(strategy, actor string) error {
	if !akun.ValidStrategy(strategy) {
		return fmt.Errorf("%w: strategi %q tidak dikenal", ErrAccountInvalid, strategy)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.strategy
	p.strategy = strategy
	if err := p.saveLocked(true); err != nil {
		p.strategy = old
		return err
	}
	for _, a := range p.accounts {
		a.swrr = 0
	}
	log.Printf("copilotd: strategi pembagian %s → %s oleh %s", old, strategy, actorOrDash(cleanActor(actor)))
	p.notify()
	return nil
}

func (p *Pool) activeCountLocked() int {
	n := 0
	for _, a := range p.accounts {
		a.mu.Lock()
		if a.role != akun.RoleNonaktif {
			n++
		}
		a.mu.Unlock()
	}
	return n
}

// saveLocked menulis daftar akun ke akun.json (pemanggil memegang p.mu). Tanpa StateDir perubahan hanya di memori.
func (p *Pool) saveLocked(backup bool) error {
	if p.storePath == "" {
		return nil
	}
	if p.storeErr != "" {
		return fmt.Errorf("%w: %s tidak terbaca (%s); perbaiki atau pulihkan dari folder cadangan lalu restart copilotd",
			ErrStoreUnavailable, akun.FileName, p.storeErr)
	}
	records := make([]akun.Record, 0, len(p.accounts))
	for _, a := range p.accounts {
		a.mu.Lock()
		records = append(records, akun.Record{
			ID: a.id, Token: a.token, Source: a.source, Role: a.role, Label: a.label, AddedAt: a.addedAt,
			AddedBy: a.addedBy, Identity: a.identity,
		})
		a.mu.Unlock()
	}
	if err := akun.Save(p.storePath, akun.File{Strategy: p.strategy, Accounts: records}, backup); err != nil {
		return fmt.Errorf("menyimpan daftar akun: %w", err)
	}
	p.managed = true
	return nil
}

func cleanLabel(raw string) (string, error) {
	label := strings.Join(strings.FieldsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len([]rune(label)) > maxLabel {
		return "", fmt.Errorf("%w: label maksimal %d karakter", ErrAccountInvalid, maxLabel)
	}
	return label, nil
}

func cleanActor(raw string) string {
	actor := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	if r := []rune(actor); len(r) > 200 {
		actor = string(r[:200])
	}
	return actor
}

func actorOrDash(actor string) string {
	if actor == "" {
		return "-"
	}
	return actor
}

// reset melepas dan mematikan semua runtime akun, termasuk runtime lama yang menunggu permintaannya selesai.
func (a *account) reset() {
	a.mu.Lock()
	clients := append([]*copilot.Client{a.client}, a.draining...)
	a.detachLocked()
	a.draining = nil
	a.mu.Unlock()
	a.halt(clients...)
}

// drop membuang client yang koneksinya rusak (nil = runtime yang terpasang). Runtime lain milik akun tidak ikut
// dimatikan: runtime pengganti yang sudah terpasang, atau runtime lama yang masih melayani permintaan lain.
func (a *account) drop(client *copilot.Client) {
	a.mu.Lock()
	if client != nil && a.client != client {
		a.mu.Unlock()
		return
	}
	client = a.client
	a.detachLocked()
	a.mu.Unlock()
	a.halt(client)
}

func (a *account) detachLocked() {
	a.client, a.rpcAPI = nil, nil
	a.startedAt, a.modelsAt = time.Time{}, time.Time{}
}

// halt mematikan runtime yang sudah dilepas dari akun.
func (a *account) halt(clients ...*copilot.Client) {
	for _, c := range clients {
		switch {
		case c == nil:
		case a.stop != nil:
			a.stop(c)
		default:
			_ = c.Stop()
		}
	}
}

func (a *account) current() *copilot.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// ensure menyalakan runtime bila belum ada (started=true bila baru dinyalakan). Start memakan beberapa detik, jadi
// client-nya dipakai ulang selama runtime hidup; mu tidak ditahan selama start supaya status/pick tidak ikut macet,
// dan permintaan tidak menunggu startMu selama sudah ada runtime terpasang (mis. saat penggantinya dinyalakan).
func (p *Pool) ensure(ctx context.Context, a *account) (*copilot.Client, bool, error) {
	if client := a.current(); client != nil {
		return client, false, nil
	}
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if client := a.current(); client != nil {
		return client, false, nil
	}
	client, api, err := p.spawn(ctx, p.cfg, a.token)
	if err != nil {
		return nil, false, err
	}
	if err := a.attach(client, api, p.now()); err != nil {
		a.halt(client)
		return nil, false, err
	}
	return client, true, nil
}

// startRuntime menyalakan satu runtime Copilot untuk akun bertoken itu (token kosong = akun login tersimpan).
func startRuntime(ctx context.Context, cfg config.Config, token string) (*copilot.Client, *rpc.ServerRPC, error) {
	opts := &copilot.ClientOptions{
		LogLevel:         cfg.LogLevel,
		WorkingDirectory: cfg.WorkingDir,
		BaseDirectory:    cfg.BaseDir,
	}
	if token != "" {
		opts.GitHubToken = token
	} else {
		opts.UseLoggedInUser = copilot.Bool(true)
	}
	if conn := connection(cfg); conn != nil {
		opts.Connection = conn
	}

	client := copilot.NewClient(opts)
	if err := client.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("runtime Copilot gagal start: %s", redact(err.Error(), token))
	}
	return client, client.RPC, nil
}

// recycle mengganti runtime akun dengan runtime baru supaya kuota dan daftar model dibaca ulang dari layanan
// Copilot. Runtime baru dinyalakan dulu dan baru dipasang bila berhasil start, jadi kegagalan tidak mengganggu
// layanan; runtime lama dimatikan setelah permintaan yang masih memakainya selesai. Mengembalikan false tanpa
// berbuat apa-apa bila akun tidak punya runtime hidup atau runtime-nya lebih muda dari minAge.
func (p *Pool) recycle(ctx context.Context, a *account, minAge time.Duration) (bool, error) {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	a.mu.Lock()
	age := p.now().Sub(a.startedAt)
	skip := a.client == nil || a.removed || age < minAge
	a.mu.Unlock()
	if skip {
		return false, nil
	}
	client, api, err := p.spawn(ctx, p.cfg, a.token)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	if a.removed {
		a.mu.Unlock()
		a.halt(client)
		return false, errAccountRemoved
	}
	// a.client bisa sudah nil di sini (koneksi runtime lama putus selama start); runtime baru tetap dipasang.
	old := a.client
	a.client, a.rpcAPI = client, api
	a.startedAt, a.modelsAt = p.now(), time.Time{}
	a.noModel = nil
	idle := len(a.slots) == 0
	if old != nil && !idle {
		a.draining = append(a.draining, old)
	}
	a.mu.Unlock()
	if old != nil && idle {
		go a.halt(old)
	}
	log.Printf("copilotd: runtime akun %s diganti runtime baru (runtime lama hidup %s)", a.logName(), age.Round(time.Second))
	return true, nil
}

// RecycleRuntimes mengganti runtime semua akun yang boleh dipakai dan sudah hidup lebih dari recycleMinAge, lalu
// membaca ulang identitas dan kuotanya di latar. Galat yang dikembalikan menyebut akun yang runtime penggantinya
// gagal start; runtime lama akun itu tetap dipakai.
func (p *Pool) RecycleRuntimes(ctx context.Context) error {
	var errs []error
	for _, a := range p.list() {
		a.mu.Lock()
		disabled := a.role == akun.RoleNonaktif
		a.mu.Unlock()
		if disabled {
			continue
		}
		done, err := p.recycle(ctx, a, recycleMinAge)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("akun %s: %w", a.logName(), err))
		case done:
			p.afterStart(a)
		}
	}
	return errors.Join(errs...)
}

// recycleAged mengganti runtime yang umurnya melewati RuntimeMaxAge. Akun nonaktif dilewati; akun yang runtime
// penggantinya gagal start baru dicoba lagi setelah recycleRetryAfter.
func (p *Pool) recycleAged() {
	maxAge := p.cfg.RuntimeMaxAge
	if maxAge <= 0 {
		return
	}
	for _, a := range p.list() {
		a.mu.Lock()
		due := a.role != akun.RoleNonaktif && !p.now().Before(a.recycleRetryAt)
		a.mu.Unlock()
		if !due {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), recycleTimeout)
		done, err := p.recycle(ctx, a, maxAge)
		cancel()
		switch {
		case err != nil:
			a.mu.Lock()
			a.recycleRetryAt = p.now().Add(recycleRetryAfter)
			a.mu.Unlock()
			log.Printf("copilotd: runtime pengganti akun %s gagal start — %v; runtime lama tetap dipakai, dicoba lagi %s lagi",
				a.logName(), err, recycleRetryAfter)
		case done:
			p.afterStart(a)
		}
	}
}

// noteModels mencatat bahwa daftar model baru dibaca lewat client itu dan mengembalikan kapan daftar tersebut
// pertama dibaca serta kapan runtime-nya menyala. SDK menyimpan daftar model selama client hidup, jadi pembacaan
// berikutnya lewat client yang sama mengembalikan daftar yang sama.
func (a *account) noteModels(client *copilot.Client, now time.Time) (readAt, startedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != client {
		return now, time.Time{}
	}
	if a.modelsAt.IsZero() {
		a.modelsAt = now
	}
	return a.modelsAt, a.startedAt
}

// errAccountRemoved: akun dikeluarkan dari pool selagi runtime-nya dinyalakan.
var errAccountRemoved = errors.New("akun sudah dikeluarkan dari pool")

// attach memasang runtime yang baru start, kecuali akun sudah dihapus: Remove hanya mematikan runtime yang ada saat
// itu, jadi runtime yang selesai start sesudahnya harus ditolak di sini (token akun terhapus tidak boleh dipakai).
func (a *account) attach(client *copilot.Client, api *rpc.ServerRPC, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.removed {
		return errAccountRemoved
	}
	a.client, a.rpcAPI = client, api
	a.startedAt, a.modelsAt = at, time.Time{}
	return nil
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
	for _, acc := range p.list() {
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

// isAuthError: token dicabut/kedaluwarsa atau akun tidak punya akses Copilot (errorType authentication /
// authorization dari sesi). Angka 401/403 sengaja tidak dipakai sebagai penanda: bisa muncul di angka lain.
func isAuthError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"authentication", "authorization", "unauthorized", "bad credentials",
		"not authenticated", "forbidden"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isModelUnavailable: akun ini tidak menyediakan model yang diminta (mis. `Model "x" is not available.`).
func isModelUnavailable(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "model") && (strings.Contains(msg, "is not available") || strings.Contains(msg, "not supported"))
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
