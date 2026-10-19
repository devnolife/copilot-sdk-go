package runtime

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/devnolife/copilot-sdk-go/internal/config"
	"github.com/devnolife/copilot-sdk-go/internal/racikan"
)

func strp(v string) *string { return &v }

func TestRacikanOverridesTierModelsWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{StateDir: dir, Model: "gpt-6.1-sol", CheapModel: "claude-sonnet-4.6", HeavyModel: "claude-opus-5",
		AgentModel: "claude-sonnet-5", ModelFallbacks: []string{"claude-sonnet-5", "claude-opus-4.8"}}
	svc := newTestService(cfg)
	if st := svc.Racikan(); st.Stored || !st.Persistent || st.Effective.HeavyModel != "claude-opus-5" {
		t.Fatalf("awal harus murni .env: %+v", st)
	}
	fb := []string{"gpt-6-luna", "claude-haiku-4.5"}
	st, err := svc.SetRacikan(racikan.Override{CheapModel: strp("gpt-6-luna"), HeavyModel: strp("gpt-6-luna"),
		AgentModel: strp(""), Fallbacks: &fb}, "admin:uji")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Stored || st.UpdatedBy != "admin:uji" || st.UpdatedAt == nil || st.Env.HeavyModel != "claude-opus-5" {
		t.Errorf("state setelah simpan salah: %+v", st)
	}
	cases := []struct {
		req  Request
		want []string
	}{
		{Request{}, []string{"gpt-6.1-sol", "gpt-6-luna", "claude-haiku-4.5"}},
		{Request{Tier: "heavy"}, []string{"gpt-6-luna", "claude-haiku-4.5"}},
		{Request{Tier: "agent"}, []string{"gpt-6.1-sol", "gpt-6-luna", "claude-haiku-4.5"}},
		{Request{Tier: "cheap"}, []string{"gpt-6-luna", "claude-haiku-4.5"}},
		{Request{Model: "kimi-k3"}, []string{"kimi-k3"}},
	}
	for _, tc := range cases {
		if got := svc.modelChain(tc.req); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("modelChain(%+v) = %v, ingin %v", tc.req, got, tc.want)
		}
	}

	// Restart: racikan.json terbaca lagi.
	again := newTestService(cfg)
	if got := again.modelChain(Request{Tier: "heavy"}); !reflect.DeepEqual(got, []string{"gpt-6-luna", "claude-haiku-4.5"}) {
		t.Errorf("setelah restart modelChain heavy = %v", got)
	}

	// Override kosong = kembali ke .env.
	if _, err := again.SetRacikan(racikan.Override{}, "admin:uji"); err != nil {
		t.Fatal(err)
	}
	if got := again.modelChain(Request{Tier: "heavy"}); !reflect.DeepEqual(got, []string{"claude-opus-5", "claude-sonnet-5", "claude-opus-4.8"}) {
		t.Errorf("setelah kembali ke .env modelChain heavy = %v", got)
	}
}

func TestRacikanBrokenFileFallsBackToEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(racikan.PathIn(dir), []byte("{rusak"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(config.Config{StateDir: dir, Model: "gpt-6.1-sol"})
	st := svc.Racikan()
	if st.Error == "" || st.Stored || st.Effective.Model != "gpt-6.1-sol" {
		t.Errorf("berkas rusak harus dilaporkan dan .env dipakai: %+v", st)
	}
	if _, err := svc.SetRacikan(racikan.Override{Model: strp("gpt-6-luna")}, "admin:uji"); err != nil {
		t.Fatal(err)
	}
	if st := svc.Racikan(); st.Error != "" || svc.TierModels().Model != "gpt-6-luna" {
		t.Errorf("simpan baru harus menggantikan berkas rusak: %+v", st)
	}
}

func TestRacikanWithoutStateDirIsReadOnly(t *testing.T) {
	svc := newTestService(config.Config{Model: "gpt-6.1-sol"})
	if _, err := svc.SetRacikan(racikan.Override{Model: strp("gpt-6-luna")}, "admin:uji"); !errors.Is(err, ErrRacikanReadOnly) {
		t.Errorf("tanpa StateDir harus ErrRacikanReadOnly, dapat %v", err)
	}
	if svc.TierModels().Model != "gpt-6.1-sol" || svc.Racikan().Persistent {
		t.Error("tanpa StateDir model tetap dari .env")
	}
	if _, err := svc.SetRacikan(racikan.Override{Model: strp("a b")}, "admin:uji"); err == nil || errors.Is(err, ErrRacikanReadOnly) {
		t.Errorf("isian tak sah harus ditolak validasi lebih dulu, dapat %v", err)
	}
}
