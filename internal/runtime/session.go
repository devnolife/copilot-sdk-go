package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/config"
	"github.com/devnolife/copilot-sdk-go/internal/racikan"
)

// noTools adalah nama tool yang tidak pernah ada di registry runtime.
// Mengirimnya sebagai satu-satunya AvailableTools mengunci seluruh tool bawaan
// (shell, edit file, fetch) sehingga sesi benar-benar tersandbox.
const noTools = "__none__"

var webTools = []string{"web_search", "web_fetch"}

// jsonInstruction ditambahkan ke system prompt saat pemanggil minta JSON.
const jsonInstruction = "Balas HANYA dengan JSON valid, tanpa teks lain dan tanpa pagar kode."

// Event adalah satu kejadian selama sesi berlangsung.
type Event struct {
	Type      string         `json:"type"`
	Content   string         `json:"content,omitempty"`
	Name      string         `json:"name,omitempty"`
	Status    string         `json:"status,omitempty"`
	Model     string         `json:"model,omitempty"`
	Message   string         `json:"message,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	// Calls hanya diisi pada event "done" saat sesi memakai tool.
	Calls []ToolCall `json:"calls,omitempty"`
	// Usage hanya diisi pada event "done": satu entri per panggilan API model dalam sesi.
	Usage []UsageCall `json:"usage,omitempty"`
}

// UsageCall adalah pemakaian satu panggilan API model menurut event assistant.usage.
type UsageCall struct {
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	Cost             float64 `json:"cost"`
	NanoAiu          float64 `json:"nano_aiu"`
	DurationMs       int64   `json:"duration_ms"`
	Initiator        string  `json:"initiator,omitempty"`
	ReasoningEffort  string  `json:"reasoning_effort,omitempty"`
}

func usageCall(d *copilot.AssistantUsageData) UsageCall {
	call := UsageCall{
		Model:            d.Model,
		InputTokens:      deref(d.InputTokens),
		OutputTokens:     deref(d.OutputTokens),
		CacheReadTokens:  deref(d.CacheReadTokens),
		CacheWriteTokens: deref(d.CacheWriteTokens),
		ReasoningTokens:  deref(d.ReasoningTokens),
		Cost:             deref(d.Cost),
		DurationMs:       deref(d.Duration),
		Initiator:        deref(d.Initiator),
		ReasoningEffort:  deref(d.ReasoningEffort),
	}
	if d.CopilotUsage != nil {
		call.NanoAiu = d.CopilotUsage.TotalNanoAiu
	}
	return call
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// Request adalah parameter umum satu pemanggilan.
type Request struct {
	Prompt          string
	System          string
	Model           string
	Tier            string
	JSONMode        bool
	Web             bool
	Timeout         time.Duration
	Managed         bool
	MaxAiCredits    float64
	MaxOutputTokens int64
	MaxToolCalls    int
	// ReasoningEffort & ContextTier hanya berlaku untuk model yang diminta eksplisit (Model).
	ReasoningEffort string
	ContextTier     string
}

// ReasoningEfforts adalah tingkat penalaran yang dikenal SDK, dari ringan ke berat.
var ReasoningEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// ValidReasoningEffort: kosong (bawaan model) atau salah satu ReasoningEfforts.
func ValidReasoningEffort(v string) bool {
	return v == "" || slices.Contains(ReasoningEfforts, v)
}

// ValidContextTier: kosong (perilaku normal), "default", atau "long_context".
func ValidContextTier(v string) bool {
	return v == "" || v == string(copilot.ContextTierDefault) || v == string(copilot.ContextTierLongContext)
}

// ToolSpec adalah deklarasi tool yang dieksekusi di sisi pemanggil.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolExecutor menjalankan tool dan mengembalikan hasilnya sebagai teks.
type ToolExecutor func(ctx context.Context, name string, args map[string]any) (string, error)

// Service membungkus pool menjadi operasi tingkat tinggi.
type Service struct {
	cfg  config.Config
	pool *Pool

	mu   sync.Mutex
	stop chan struct{}

	// racikan: model tiap tier yang diatur admin (racikan.json), menimpa .env.
	racikanMu     sync.RWMutex
	racikan       racikan.File
	racikanStored bool
	racikanErr    string
}

// Options mengganti bagian service untuk tes.
type Options struct {
	// Probe memeriksa token akun baru (bawaan ProbeAccount, yang menyalakan runtime sementara).
	Probe func(ctx context.Context, cfg config.Config, token string) (AccountProbe, error)
}

// NewService menyiapkan service beserta pool akunnya.
func NewService(cfg config.Config) *Service {
	s := &Service{cfg: cfg, pool: NewPool(cfg)}
	s.loadRacikan()
	return s
}

// NewServiceWithOptions seperti NewService dengan kait pengganti.
func NewServiceWithOptions(cfg config.Config, opts Options) *Service {
	s := NewService(cfg)
	if opts.Probe != nil {
		s.pool.probe = opts.Probe
	}
	return s
}

// Config mengembalikan konfigurasi aktif.
func (s *Service) Config() config.Config { return s.cfg }

// Accounts melaporkan keadaan pool akun (tanpa token, tanpa menyalakan runtime).
func (s *Service) Accounts() []AccountStatus { return s.pool.Status() }

// AccountCount, PoolStrategy, dan PoolSource meringkas pool untuk status dan log.
func (s *Service) AccountCount() int    { return s.pool.Size() }
func (s *Service) PoolStrategy() string { return s.pool.Strategy() }
func (s *Service) PoolSource() string   { return s.pool.Source() }

// Overview melaporkan akun, pemakaian, dan perkiraan kuota untuk admin.
func (s *Service) Overview(days int) Overview { return s.pool.Overview(days) }

// AddAccount menambahkan akun baru ke pool (berlaku tanpa restart).
func (s *Service) AddAccount(ctx context.Context, in AddInput) (AccountStatus, error) {
	return s.pool.Add(ctx, in)
}

// UpdateAccount mengubah peran/label akun.
func (s *Service) UpdateAccount(id string, in UpdateInput) (AccountStatus, error) {
	return s.pool.Update(id, in)
}

// RemoveAccount mengeluarkan akun dari pool.
func (s *Service) RemoveAccount(id, actor string) error { return s.pool.Remove(id, actor) }

// RefreshAccounts membaca ulang identitas dan kuota sekarang juga (id kosong = semua akun).
func (s *Service) RefreshAccounts(ctx context.Context, id string) error {
	return s.pool.Refresh(ctx, id)
}

// SetStrategy mengganti strategi pembagian permintaan.
func (s *Service) SetStrategy(strategy, actor string) error {
	return s.pool.SetStrategy(strategy, actor)
}

// Start menjalankan pekerjaan latar (catatan pemakaian, pembacaan kuota berkala). Aman dipanggil berkali-kali.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return
	}
	s.stop = make(chan struct{})
	go s.pool.run(s.stop)
}

// Shutdown menghentikan pekerjaan latar, menyimpan catatan pemakaian, lalu mematikan seluruh runtime.
func (s *Service) Shutdown() {
	s.mu.Lock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	s.mu.Unlock()
	if err := s.pool.usage.flush(time.Now()); err != nil {
		log.Printf("copilotd: menyimpan catatan pemakaian: %v", err)
	}
	s.pool.Shutdown()
}

// Models mengembalikan ID model yang bisa dipakai akun ini.
func (s *Service) Models(ctx context.Context) ([]copilot.ModelInfo, error) {
	lease, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	models, err := lease.Client.ListModels(ctx)
	if err != nil {
		lease.Failed(err)
		return nil, fmt.Errorf("gagal mengambil daftar model: %w", err)
	}
	return models, nil
}

// resolveModel memilih model: eksplisit → tier → default (racikan admin, lalu .env).
func (s *Service) resolveModel(req Request) string {
	if req.Model != "" {
		return req.Model
	}
	return s.TierModels().For(req.Tier)
}

// modelChain adalah urutan model yang dicoba: pilihan utama lalu fallback.
// Fallback hanya dipakai kalau pemanggil tidak memaksa model tertentu.
func (s *Service) modelChain(req Request) []string {
	models := s.TierModels()
	primary := req.Model
	if primary == "" {
		primary = models.For(req.Tier)
	}
	chain := []string{primary}
	if req.Model != "" || req.Managed {
		return chain
	}
	for _, fb := range models.Fallbacks {
		if fb != primary {
			chain = append(chain, fb)
		}
	}
	return chain
}

func (s *Service) timeout(req Request, fallback time.Duration) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return fallback
}

// sessionConfig menyusun konfigurasi sesi sesuai mode yang diminta.
func (s *Service) sessionConfig(req Request, model string, tools []copilot.Tool) *copilot.SessionConfig {
	cfg := &copilot.SessionConfig{
		Model:     model,
		Streaming: copilot.Bool(true),
	}
	if req.Model != "" && model == req.Model {
		cfg.ReasoningEffort = req.ReasoningEffort
		if req.ContextTier != "" {
			cfg.ContextTier = copilot.ContextTier(req.ContextTier)
		}
	}
	if req.Managed {
		cfg.SessionLimits = &rpc.SessionLimitsConfig{MaxAiCredits: &req.MaxAiCredits}
		cfg.ModelCapabilities = &rpc.ModelCapabilitiesOverride{
			Limits: &rpc.ModelCapabilitiesOverrideLimits{MaxOutputTokens: &req.MaxOutputTokens},
		}
		cfg.EnableFileHooks = copilot.Bool(false)
		cfg.SkipCustomInstructions = copilot.Bool(true)
		cfg.CustomAgentsLocalOnly = copilot.Bool(true)
		cfg.CoauthorEnabled = copilot.Bool(false)
		cfg.ManageScheduleEnabled = copilot.Bool(false)
		var mu sync.Mutex
		calls := 0
		cfg.Hooks = &copilot.SessionHooks{
			OnPreToolUse: func(_ copilot.PreToolUseHookInput, _ copilot.HookInvocation) (*copilot.PreToolUseHookOutput, error) {
				mu.Lock()
				defer mu.Unlock()
				if calls >= req.MaxToolCalls {
					return &copilot.PreToolUseHookOutput{PermissionDecision: "deny", PermissionDecisionReason: "generation_tool_budget_exceeded"}, nil
				}
				calls++
				return &copilot.PreToolUseHookOutput{PermissionDecision: "allow"}, nil
			},
		}
	}

	switch {
	case len(tools) > 0:
		// Hanya tool milik pemanggil yang tersedia, jadi approve-all aman.
		cfg.Tools = tools
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		cfg.AvailableTools = names
		cfg.OnPermissionRequest = approveAll
	case req.Web:
		cfg.AvailableTools = webTools
		cfg.OnPermissionRequest = approveAll
	default:
		cfg.AvailableTools = []string{noTools}
		cfg.OnPermissionRequest = denyAll
	}

	system := req.System
	if req.JSONMode {
		if system == "" {
			system = jsonInstruction
		} else {
			system = system + "\n\n" + jsonInstruction
		}
	}
	if system != "" {
		cfg.SystemMessage = &copilot.SystemMessageConfig{Mode: "replace", Content: system}
	}

	if s.cfg.IsBYOK() {
		cfg.Provider = &copilot.ProviderConfig{
			Type:    s.cfg.ProviderType,
			BaseURL: s.cfg.ProviderBaseURL,
			APIKey:  s.cfg.ProviderAPIKey,
			WireAPI: s.cfg.ProviderWireAPI,
		}
	}
	return cfg
}

func approveAll(copilot.PermissionRequest, copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	return &rpc.PermissionDecisionApproveOnce{}, nil
}

func denyAll(copilot.PermissionRequest, copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	return &rpc.PermissionDecisionReject{}, nil
}

// Stream menjalankan satu giliran percakapan dan mengalirkan event ke emit.
//
// Kalau akun kena rate limit atau kuotanya habis sebelum ada teks yang terkirim, model yang sama dicoba lagi di
// akun lain (akun yang gagal sudah masuk cooldown). Model berikutnya di modelChain baru dicoba bila tidak ada akun
// lain yang tersedia. Permintaan managed tidak diulang karena batas waktunya mencakup seluruh permintaan.
func (s *Service) Stream(ctx context.Context, req Request, tools []ToolSpec, exec ToolExecutor, emit func(Event)) error {
	if req.Managed && (req.MaxAiCredits <= 0 || req.MaxOutputTokens <= 0 || req.MaxToolCalls < 0 || req.Timeout <= 0) {
		return fmt.Errorf("managed generation requires explicit limits")
	}
	return runChain(s.modelChain(req), !req.Managed, s.pool.Size(), s.pool.HasAvailable, func(model string) error {
		return s.runOnce(ctx, req, model, tools, exec, emit)
	})
}

// runChain mencoba tiap model; kegagalan yang layak diulang (errRetryable) dicoba lagi dengan model yang sama di
// akun lain selama switchAccounts dan available(), paling banyak sekali per akun, baru pindah ke model berikutnya.
func runChain(models []string, switchAccounts bool, accounts int, available func() bool, run func(model string) error) error {
	var lastErr error
	for _, model := range models {
		for attempt := 0; attempt < max(1, accounts); attempt++ {
			err := run(model)
			if err == nil {
				return nil
			}
			lastErr = err
			if !errors.Is(err, errRetryable) {
				return err
			}
			if !switchAccounts || !available() {
				break
			}
		}
	}
	return lastErr
}

// errRetryable menandai kegagalan yang layak dicoba dengan model lain.
var errRetryable = errors.New("retryable")

func (s *Service) runOnce(
	ctx context.Context,
	req Request,
	model string,
	tools []ToolSpec,
	exec ToolExecutor,
	emit func(Event),
) (runErr error) {
	deadline := s.timeout(req, s.cfg.Timeout)
	if len(tools) > 0 {
		deadline = s.timeout(req, s.cfg.AgentTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	lease, err := s.pool.AcquireModel(ctx, model)
	if err != nil {
		return err
	}
	var (
		mu    sync.Mutex
		usage []UsageCall
	)
	defer func() {
		mu.Lock()
		calls := slices.Clone(usage)
		mu.Unlock()
		// Hasil (berhasil/gagal + pemakaian token) dicatat ke akun yang melayani, untuk laporan admin.
		lease.Done(runErr, calls)
		lease.Release()
	}()

	sdkTools, calls := buildTools(ctx, tools, exec, emit)
	cfg := s.sessionConfig(req, model, sdkTools)
	// Saat tool milik pemanggil terdaftar, handler-nya sudah memancarkan
	// event sendiri (lengkap dengan argumen). Event dari runtime dilewati
	// supaya klien tidak menerima progres ganda.
	ownTools := len(sdkTools) > 0

	session, err := lease.Client.CreateSession(ctx, cfg)
	if err != nil {
		if isModelUnavailable(err) {
			// Daftar model bisa berbeda antar-akun: coba model yang sama di akun lain.
			if lease.ModelUnavailable() {
				return fmt.Errorf("%w: %v", errRetryable, err)
			}
			return fmt.Errorf("gagal membuat sesi: %w", err)
		}
		lease.Failed(err)
		if isRateLimit(err) || isAuthError(err) {
			return fmt.Errorf("%w: %v", errRetryable, err)
		}
		return fmt.Errorf("gagal membuat sesi: %w", err)
	}
	defer session.Disconnect()

	var (
		final    string
		failure  string
		limited  string // jenis galat sesi yang layak dicoba di akun lain: rate_limit | quota | authentication | authorization
		streamed bool
		once     sync.Once
	)
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }

	unsubscribe := session.On(func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			if d.DeltaContent == "" {
				return
			}
			mu.Lock()
			streamed = true
			mu.Unlock()
			emit(Event{Type: "delta", Content: d.DeltaContent})
		case *copilot.AssistantReasoningDeltaData:
			if d.DeltaContent != "" {
				emit(Event{Type: "reasoning", Content: d.DeltaContent})
			}
		case *copilot.ToolExecutionStartData:
			if !ownTools {
				emit(Event{Type: "tool", Status: "start", Name: d.ToolName})
			}
		case *copilot.ToolExecutionCompleteData:
			if !ownTools {
				emit(Event{Type: "tool", Status: "done"})
			}
		case *copilot.AssistantMessageData:
			mu.Lock()
			final = d.Content
			mu.Unlock()
		case *copilot.AssistantUsageData:
			mu.Lock()
			usage = append(usage, usageCall(d))
			mu.Unlock()
		case *copilot.SessionErrorData:
			mu.Lock()
			failure = d.Message
			switch d.ErrorType {
			case "rate_limit", "quota", "authentication", "authorization":
				limited = d.ErrorType
			}
			mu.Unlock()
			finish()
		case *copilot.SessionIdleData:
			finish()
		}
	})
	defer unsubscribe()

	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: req.Prompt}); err != nil {
		lease.Failed(err)
		return fmt.Errorf("gagal mengirim prompt: %w", err)
	}

	select {
	case <-done:
	case <-ctx.Done():
		if req.Managed {
			abortCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := session.Abort(abortCtx); err != nil {
				return fmt.Errorf("generation_result_unknown: abort could not be confirmed: %w", err)
			}
		}
		return fmt.Errorf("Copilot tidak merespons dalam %s", deadline)
	}

	mu.Lock()
	text, hadStream, errMsg, wasLimited := final, streamed, failure, limited
	toolCalls := calls.snapshot()
	usageCalls := slices.Clone(usage)
	mu.Unlock()

	if errMsg != "" {
		if wasLimited != "" {
			// Jenis galat ikut supaya pool membedakan kuota habis (cooldown panjang), rate limit, dan token ditolak.
			lease.Failed(fmt.Errorf("%s: %s", wasLimited, errMsg))
		} else {
			lease.Failed(errors.New(errMsg))
		}
		if wasLimited != "" && !hadStream {
			return fmt.Errorf("%w: %s: %s", errRetryable, wasLimited, errMsg)
		}
		return errors.New(errMsg)
	}
	if text == "" && !hadStream {
		return errors.New("Copilot mengembalikan jawaban kosong")
	}

	emit(Event{
		Type:    "done",
		Content: text,
		Model:   "copilot:" + modelLabel(model),
		Calls:   toolCalls,
		Usage:   usageCalls,
	})
	return nil
}

func modelLabel(model string) string {
	if model == "" {
		return "default"
	}
	return model
}
