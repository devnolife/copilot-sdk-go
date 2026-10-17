package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

// QuotaInfo adalah kuota satu akun yang paling dekat habis di antara jenis yang dipakai sesi SDK. Sumbernya
// Account.GetQuota, API eksperimental SDK.
type QuotaInfo struct {
	Type             string  `json:"type"`
	RemainingPercent float64 `json:"remaining_percent"`
	Used             int64   `json:"used"`
	Entitlement      int64   `json:"entitlement"`
	Unlimited        bool    `json:"unlimited"`
	// OverageAllowed: setelah kuota habis permintaan tetap dilayani sebagai kelebihan pemakaian (berbayar),
	// jadi tidak ada galat yang memicu pengalihan; hanya ambang QuotaMinPercent yang mencegahnya.
	OverageAllowed bool       `json:"overage_allowed"`
	ResetDate      *time.Time `json:"reset_date,omitempty"`
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
			ResetDate:        s.ResetDate,
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

// fetchQuota membaca kuota akun lewat RPC runtime yang sudah hidup.
func fetchQuota(ctx context.Context, api *rpc.ServerRPC) (*QuotaInfo, error) {
	if api == nil || api.Account == nil {
		return nil, fmt.Errorf("runtime akun belum hidup")
	}
	res, err := api.Account.GetQuota(ctx, &rpc.AccountGetQuotaRequest{})
	if err != nil {
		return nil, err
	}
	return quotaFromSnapshots(res.QuotaSnapshots), nil
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

// AccountProbe adalah hasil pemeriksaan satu kredensial di runtime terpisah.
type AccountProbe struct {
	Kind          string     `json:"kind"`
	Authenticated bool       `json:"authenticated"`
	Login         string     `json:"login,omitempty"`
	AuthType      string     `json:"auth_type,omitempty"`
	Quota         *QuotaInfo `json:"quota,omitempty"`
	QuotaError    string     `json:"quota_error,omitempty"`
}

// ProbeAccount menyalakan runtime sementara untuk satu kredensial (token kosong = akun login tersimpan), membaca
// status login dan kuotanya, lalu mematikannya. Token tidak pernah dicetak atau dicatat.
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
		return probe, fmt.Errorf("runtime Copilot gagal start: %w", err)
	}
	defer client.Stop()
	st, err := client.GetAuthStatus(ctx)
	if err != nil {
		return probe, fmt.Errorf("status login tidak terbaca: %w", err)
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
		return probe, fmt.Errorf("tidak terautentikasi: %s", msg)
	}
	q, err := fetchQuota(ctx, client.RPC)
	if err != nil {
		probe.QuotaError = err.Error()
	} else {
		probe.Quota = q
	}
	return probe, nil
}
