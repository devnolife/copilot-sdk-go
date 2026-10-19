package racikan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/devnolife/copilot-sdk-go/internal/config"
)

func str(v string) *string { return &v }

func TestApplyNilFollowsEnvEmptyClears(t *testing.T) {
	env := FromConfig(config.Config{Model: "gpt-6.1-sol", CheapModel: "claude-haiku-4.5", HeavyModel: "claude-opus-5",
		AgentModel: "claude-sonnet-5", ModelFallbacks: []string{"claude-sonnet-5"}})
	fallbacks := []string{"gpt-6-luna", "claude-haiku-4.5"}
	got := Override{HeavyModel: str("gpt-6-luna"), AgentModel: str(""), Fallbacks: &fallbacks}.Apply(env)
	want := Models{Model: "gpt-6.1-sol", CheapModel: "claude-haiku-4.5", HeavyModel: "gpt-6-luna", AgentModel: "",
		Fallbacks: []string{"gpt-6-luna", "claude-haiku-4.5"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply = %+v, ingin %+v", got, want)
	}
	if got.For("agent") != "gpt-6.1-sol" {
		t.Errorf("agen kosong harus ikut model utama, dapat %q", got.For("agent"))
	}
	if got.For("heavy") != "gpt-6-luna" || got.For("cheap") != "claude-haiku-4.5" || got.For("") != "gpt-6.1-sol" {
		t.Errorf("For per tier salah: %+v", got)
	}
	fallbacks[0] = "diubah"
	if got.Fallbacks[0] != "gpt-6-luna" {
		t.Error("Apply harus menyalin daftar cadangan")
	}
	if (Override{}).Apply(env).Model != "gpt-6.1-sol" || !(Override{}).Empty() {
		t.Error("override kosong harus sama dengan .env")
	}
}

func TestValidate(t *testing.T) {
	five := []string{"a", "b", "c", "d", "e"}
	six := append(append([]string{}, five...), "f")
	twice := []string{"gpt-6-luna", "gpt-6-luna"}
	bad := []string{"gpt 6"}
	cases := []struct {
		name string
		o    Override
		ok   bool
	}{
		{"kosong", Override{}, true},
		{"string kosong boleh", Override{AgentModel: str("")}, true},
		{"nama biasa", Override{Model: str("claude-haiku-4.5"), HeavyModel: str("mai-code-1.1-flash")}, true},
		{"spasi ditolak", Override{Model: str("gpt 6")}, false},
		{"karakter shell ditolak", Override{CheapModel: str("x;rm")}, false},
		{"lima cadangan", Override{Fallbacks: &five}, true},
		{"enam cadangan", Override{Fallbacks: &six}, false},
		{"cadangan ganda", Override{Fallbacks: &twice}, false},
		{"cadangan tak sah", Override{Fallbacks: &bad}, false},
	}
	for _, tc := range cases {
		if err := tc.o.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v, ingin ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestSaveLoadRoundTripWithBackup(t *testing.T) {
	dir := t.TempDir()
	path := PathIn(dir)
	if _, exists, err := Load(path); exists || err != nil {
		t.Fatalf("berkas belum ada: exists=%v err=%v", exists, err)
	}
	fb := []string{"gpt-6-luna"}
	first := File{Override: Override{CheapModel: str("gpt-6-luna"), Fallbacks: &fb}, UpdatedBy: "admin:a"}
	if err := Save(path, first); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, File{Override: Override{AgentModel: str("")}, UpdatedBy: "admin:b"}); err != nil {
		t.Fatal(err)
	}
	got, exists, err := Load(path)
	if err != nil || !exists {
		t.Fatalf("Load: exists=%v err=%v", exists, err)
	}
	if got.Override.AgentModel == nil || *got.Override.AgentModel != "" || got.Override.CheapModel != nil || got.UpdatedBy != "admin:b" {
		t.Errorf("isi setelah simpan ulang salah: %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("izin racikan.json = %v (%v), ingin 0600", info.Mode().Perm(), err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "cadangan", "racikan-*.json"))
	if len(backups) != 1 {
		t.Errorf("cadangan = %d berkas, ingin 1 (isi sebelum simpan kedua)", len(backups))
	}
}

func TestLoadRejectsBrokenOrInvalid(t *testing.T) {
	dir := t.TempDir()
	path := PathIn(dir)
	if err := os.WriteFile(path, []byte("{bukan json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := Load(path); err == nil || !exists {
		t.Errorf("JSON rusak harus galat: exists=%v err=%v", exists, err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"override":{"model":"a b"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Error("nama model tak sah harus dianggap rusak")
	}
	if err := Save(path, File{Override: Override{Model: str("a b")}}); err == nil {
		t.Error("Save harus menolak isian tak sah")
	}
}
