package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/config"
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
}

// Request adalah parameter umum satu pemanggilan.
type Request struct {
	Prompt   string
	System   string
	Model    string
	Tier     string
	JSONMode bool
	Web      bool
	Timeout  time.Duration
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
}

// NewService menyiapkan service beserta pool akunnya.
func NewService(cfg config.Config) *Service {
	return &Service{cfg: cfg, pool: NewPool(cfg)}
}

// Config mengembalikan konfigurasi aktif.
func (s *Service) Config() config.Config { return s.cfg }

// Shutdown mematikan seluruh runtime.
func (s *Service) Shutdown() { s.pool.Shutdown() }

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

// resolveModel memilih model: eksplisit → tier → default konfigurasi.
func (s *Service) resolveModel(req Request) string {
	if req.Model != "" {
		return req.Model
	}
	return s.cfg.ModelFor(req.Tier)
}

// modelChain adalah urutan model yang dicoba: pilihan utama lalu fallback.
// Fallback hanya dipakai kalau pemanggil tidak memaksa model tertentu.
func (s *Service) modelChain(req Request) []string {
	primary := s.resolveModel(req)
	chain := []string{primary}
	if req.Model != "" {
		return chain
	}
	for _, fb := range s.cfg.ModelFallbacks {
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
// Model dicoba berurutan sesuai modelChain; kalau yang pertama kena rate limit
// dan belum ada teks yang terkirim, model berikutnya dicoba secara transparan.
func (s *Service) Stream(ctx context.Context, req Request, tools []ToolSpec, exec ToolExecutor, emit func(Event)) error {
	var lastErr error
	for _, model := range s.modelChain(req) {
		err := s.runOnce(ctx, req, model, tools, exec, emit)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, errRetryable) {
			return err
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
) error {
	deadline := s.timeout(req, s.cfg.Timeout)
	if len(tools) > 0 {
		deadline = s.timeout(req, s.cfg.AgentTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	lease, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer lease.Release()

	sdkTools, calls := buildTools(ctx, tools, exec, emit)
	cfg := s.sessionConfig(req, model, sdkTools)
	// Saat tool milik pemanggil terdaftar, handler-nya sudah memancarkan
	// event sendiri (lengkap dengan argumen). Event dari runtime dilewati
	// supaya klien tidak menerima progres ganda.
	ownTools := len(sdkTools) > 0

	session, err := lease.Client.CreateSession(ctx, cfg)
	if err != nil {
		lease.Failed(err)
		if isRateLimit(err) {
			return fmt.Errorf("%w: %v", errRetryable, err)
		}
		return fmt.Errorf("gagal membuat sesi: %w", err)
	}
	defer session.Disconnect()

	var (
		mu       sync.Mutex
		final    string
		failure  string
		limited  bool
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
		case *copilot.SessionErrorData:
			mu.Lock()
			failure = d.Message
			limited = d.ErrorType == "rate_limit" || d.ErrorType == "quota"
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
		return fmt.Errorf("Copilot tidak merespons dalam %s", deadline)
	}

	mu.Lock()
	text, hadStream, errMsg, wasLimited := final, streamed, failure, limited
	toolCalls := calls.snapshot()
	mu.Unlock()

	if errMsg != "" {
		lease.Failed(errors.New(errMsg))
		if wasLimited && !hadStream {
			return fmt.Errorf("%w: %s", errRetryable, errMsg)
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
	})
	return nil
}

func modelLabel(model string) string {
	if model == "" {
		return "default"
	}
	return model
}
