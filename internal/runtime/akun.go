package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/akun"
	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// QuotaInfo adalah kuota satu akun yang paling dekat habis di antara jenis yang dipakai sesi SDK. Sumbernya
// Account.GetQuota, API eksperimental SDK.
//
// Tanggal reset sengaja tidak diambil dari sini: field resetDate pada snapshot GetQuota berisi cap waktu snapshot
// (terverifikasi 19 Okt 2026), bukan tanggal reset. Tanggal reset bulanan dibaca dari GetCurrentAuth (Identity).
type QuotaInfo struct {
	Type             string  `json:"type"`
	RemainingPercent float64 `json:"remaining_percent"`
	Used             int64   `json:"used"`
	Entitlement      int64   `json:"entitlement"`
	Unlimited        bool    `json:"unlimited"`
	// OverageAllowed: setelah kuota habis permintaan tetap dilayani sebagai kelebihan pemakaian (berbayar),
	// jadi tidak ada galat yang memicu pengalihan; hanya ambang QuotaMinPercent yang mencegahnya.
	OverageAllowed bool `json:"overage_allowed"`
}

// remainingUnits: sisa unit kuota bila entitlement-nya diketahui dan berbatas.
func (q *QuotaInfo) remainingUnits() (float64, bool) {
	if q == nil || q.Unlimited || q.Entitlement <= 0 {
		return 0, false
	}
	return float64(q.Entitlement) * q.RemainingPercent / 100, true
}

// quotaTypes: jenis kuota yang menentukan "hampir habis". Kuota completions (pelengkapan kode di editor) tidak
// dipakai sesi SDK.
var quotaTypes = []string{"premium_interactions", "chat"}

// quotaFromSnapshots memilih jenis berbatas dengan sisa persen terkecil. Bila semuanya tak terbatas, jenis
// pertama yang ada dikembalikan; nil bila tidak ada jenis yang dikenal.
func quotaFromSnapshots(snaps map[string]rpc.AccountQuotaSnapshot) *QuotaInfo {
	var best *QuotaInfo
	for _, typ := range quotaTypes {
		s, ok := snaps[typ]
		if !ok {
			continue
		}
		q := &QuotaInfo{
			Type:             typ,
			RemainingPercent: s.RemainingPercentage,
			Used:             s.UsedRequests,
			Entitlement:      s.EntitlementRequests,
			Unlimited:        s.IsUnlimitedEntitlement,
			OverageAllowed:   s.UsageAllowedWithExhaustedQuota || s.OverageAllowedWithExhaustedQuota,
		}
		if best == nil || (!q.Unlimited && (best.Unlimited || q.RemainingPercent < best.RemainingPercent)) {
			best = q
		}
	}
	return best
}

// lowQuota: kuota diketahui, berbatas, dan sisanya <= ambang (ambang 0 = fitur mati).
func lowQuota(q *QuotaInfo, minPercent float64) bool {
	return q != nil && !q.Unlimited && minPercent > 0 && q.RemainingPercent <= minPercent
}

// fetchQuota membaca kuota lewat RPC runtime yang sudah hidup. token kosong = akun milik runtime itu sendiri;
// token terisi = akun lain (GetQuota{GitHubToken}), sehingga satu runtime cukup untuk membaca kuota semua akun
// bertoken.
func fetchQuota(ctx context.Context, api *rpc.ServerRPC, token string) (*QuotaInfo, error) {
	if api == nil || api.Account == nil {
		return nil, fmt.Errorf("runtime akun belum hidup")
	}
	req := &rpc.AccountGetQuotaRequest{}
	if token != "" {
		req.GitHubToken = &token
	}
	res, err := api.Account.GetQuota(ctx, req)
	if err != nil {
		return nil, errors.New(redact(err.Error(), token))
	}
	q := quotaFromSnapshots(res.QuotaSnapshots)
	if q == nil {
		return nil, fmt.Errorf("data kuota kosong")
	}
	return q, nil
}

// readIdentity membaca login, plan, dan tanggal reset kuota bulanan akun milik runtime lewat
// Account.GetCurrentAuth (GetAuthStatus tidak mengembalikan login untuk token github_pat_).
func readIdentity(ctx context.Context, api *rpc.ServerRPC) (*akun.Identity, error) {
	if api == nil || api.Account == nil {
		return nil, fmt.Errorf("runtime akun belum hidup")
	}
	res, err := api.Account.GetCurrentAuth(ctx)
	if err != nil {
		return nil, err
	}
	id := identityFromAuth(res.AuthInfo, time.Now())
	if id == nil {
		return nil, fmt.Errorf("data akun Copilot tidak tersedia")
	}
	return id, nil
}

// identityFromAuth mengambil data akun dari varian AuthInfo; token di dalamnya tidak pernah disalin.
func identityFromAuth(info rpc.AuthInfo, now time.Time) *akun.Identity {
	var user *rpc.CopilotUserResponse
	login := ""
	switch v := info.(type) {
	case *rpc.UserAuthInfo:
		user, login = v.CopilotUser, v.Login
	case *rpc.GhCLIAuthInfo:
		user, login = v.CopilotUser, v.Login
	case *rpc.EnvAuthInfo:
		user = v.CopilotUser
		if v.Login != nil {
			login = *v.Login
		}
	case *rpc.TokenAuthInfo:
		user = v.CopilotUser
	case *rpc.APIKeyAuthInfo:
		user = v.CopilotUser
	case *rpc.CopilotAPITokenAuthInfo:
		user = v.CopilotUser
	case *rpc.HMACAuthInfo:
		user = v.CopilotUser
	default:
		return nil
	}
	if user == nil && login == "" {
		return nil
	}
	checked := now.UTC()
	id := &akun.Identity{Login: login, CheckedAt: &checked}
	if user != nil {
		if user.Login != nil && *user.Login != "" {
			id.Login = *user.Login
		}
		if user.CopilotPlan != nil {
			id.Plan = *user.CopilotPlan
		}
		id.ResetAt = parseResetDate(user.QuotaResetDateUTC, user.QuotaResetDate)
		if user.TokenBasedBilling != nil {
			v := *user.TokenBasedBilling
			id.TokenBasedBilling = &v
		}
	}
	return id
}

// parseResetDate menerima "2026-11-01T00:00:00.000Z" (quota_reset_date_utc) atau "2026-11-01" (quota_reset_date).
func parseResetDate(values ...*string) *time.Time {
	for _, s := range values {
		if s == nil || *s == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
			if t, err := time.Parse(layout, *s); err == nil {
				t = t.UTC()
				return &t
			}
		}
	}
	return nil
}

// TokenKind memberi jenis kredensial tanpa membuka isinya.
func TokenKind(token string) string {
	switch {
	case token == "":
		return "login"
	case strings.HasPrefix(token, "github_pat_"):
		return "pat-berbutir"
	case strings.HasPrefix(token, "gho_"):
		return "oauth"
	case strings.HasPrefix(token, "ghu_"):
		return "app-pengguna"
	case strings.HasPrefix(token, "ghp_"):
		return "pat-klasik"
	default:
		return "token"
	}
}

// validateNewToken memeriksa bentuk token akun baru sebelum runtime dinyalakan. Jenis yang diterima Copilot CLI
// (dokumentasi GitHub): github_pat_ berizin Copilot Requests, gho_, ghu_. PAT klasik ghp_ tidak didukung.
func validateNewToken(token string) error {
	switch {
	case token == "":
		return fmt.Errorf("%w: token wajib diisi", ErrAccountInvalid)
	case len(token) > 512:
		return fmt.Errorf("%w: token terlalu panjang", ErrAccountInvalid)
	case strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return fmt.Errorf("%w: token tidak boleh berisi spasi atau baris baru", ErrAccountInvalid)
	}
	switch TokenKind(token) {
	case "pat-klasik":
		return fmt.Errorf("%w: PAT klasik (ghp_…) tidak didukung Copilot; pakai PAT berbutir (github_pat_…) dengan izin akun Copilot Requests", ErrAccountInvalid)
	case "token":
		return fmt.Errorf("%w: jenis token tidak dikenal; pakai github_pat_…, gho_…, atau ghu_…", ErrAccountInvalid)
	}
	return nil
}

// redact menyamarkan token bila sempat muncul di pesan galat dari runtime.
func redact(msg, token string) string {
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "***")
}

// AccountProbe adalah hasil pemeriksaan satu kredensial di runtime terpisah.
type AccountProbe struct {
	Kind          string     `json:"kind"`
	Authenticated bool       `json:"authenticated"`
	Login         string     `json:"login,omitempty"`
	AuthType      string     `json:"auth_type,omitempty"`
	Plan          string     `json:"plan,omitempty"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	// TokenBasedBilling: akun ditagih per pemakaian (AI credit), bukan kuota permintaan premium tetap.
	TokenBasedBilling *bool      `json:"token_based_billing,omitempty"`
	Quota             *QuotaInfo `json:"quota,omitempty"`
	QuotaError        string     `json:"quota_error,omitempty"`
}

// identity mengubah hasil probe menjadi data akun yang disimpan.
func (p AccountProbe) identity(now time.Time) akun.Identity {
	checked := now.UTC()
	return akun.Identity{Login: p.Login, Plan: p.Plan, ResetAt: p.ResetAt, TokenBasedBilling: p.TokenBasedBilling,
		CheckedAt: &checked}
}

// ProbeAccount menyalakan runtime sementara untuk satu kredensial (token kosong = akun login tersimpan), membaca
// status login, identitas, dan kuotanya, lalu mematikannya. Token tidak pernah dicetak atau dicatat.
func ProbeAccount(ctx context.Context, cfg config.Config, token string) (AccountProbe, error) {
	probe := AccountProbe{Kind: TokenKind(token)}
	if strings.HasPrefix(token, "ghp_") {
		return probe, fmt.Errorf("PAT klasik (ghp_…) tidak didukung Copilot CLI; pakai PAT berbutir (github_pat_…) dengan izin Copilot Requests")
	}
	opts := &copilot.ClientOptions{LogLevel: cfg.LogLevel, WorkingDirectory: cfg.WorkingDir, BaseDirectory: cfg.BaseDir}
	if token != "" {
		opts.GitHubToken = token
	} else {
		opts.UseLoggedInUser = copilot.Bool(true)
	}
	if conn := connection(cfg); conn != nil {
		if _, external := conn.(copilot.URIConnection); external {
			return probe, fmt.Errorf("TURNITIN_COPILOT_CLI_URL diisi: runtime eksternal memakai autentikasinya sendiri, pool akun tidak berlaku")
		}
		opts.Connection = conn
	}
	client := copilot.NewClient(opts)
	if err := client.Start(ctx); err != nil {
		return probe, fmt.Errorf("runtime Copilot gagal start: %s", redact(err.Error(), token))
	}
	defer client.Stop()
	st, err := client.GetAuthStatus(ctx)
	if err != nil {
		return probe, fmt.Errorf("status login tidak terbaca: %s", redact(err.Error(), token))
	}
	probe.Authenticated = st.IsAuthenticated
	if st.Login != nil {
		probe.Login = *st.Login
	}
	if st.AuthType != nil {
		probe.AuthType = *st.AuthType
	}
	if !st.IsAuthenticated {
		msg := "kredensial ditolak"
		if st.StatusMessage != nil && *st.StatusMessage != "" {
			msg = *st.StatusMessage
		}
		return probe, fmt.Errorf("tidak terautentikasi: %s", redact(msg, token))
	}
	if id, err := readIdentity(ctx, client.RPC); err == nil {
		if id.Login != "" {
			probe.Login = id.Login
		}
		probe.Plan, probe.ResetAt, probe.TokenBasedBilling = id.Plan, id.ResetAt, id.TokenBasedBilling
	}
	q, err := fetchQuota(ctx, client.RPC, "")
	if err != nil {
		probe.QuotaError = err.Error()
	} else {
		probe.Quota = q
	}
	return probe, nil
}
