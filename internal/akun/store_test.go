package akun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFingerprintStableAndNeverRevealsToken(t *testing.T) {
	token := "github_pat_rahasiaSEKALI123"
	id := Fingerprint(token)
	if len(id) != 12 || strings.Contains(token, id) || id != Fingerprint(token) {
		t.Fatalf("Fingerprint = %q", id)
	}
	if Fingerprint("") != LoginID || Fingerprint("gho_lain") == id {
		t.Fatal("login harus LoginID dan token berbeda harus beda id")
	}
}

func TestFromTokensMapsLoginEntryAndDropsDuplicates(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	f := FromTokens([]string{"login", "github_pat_a", "", "github_pat_a"}, "aneh", now)
	if f.Strategy != StrategyProporsional {
		t.Fatalf("strategi asing harus jadi bawaan, dapat %q", f.Strategy)
	}
	if len(f.Accounts) != 2 {
		t.Fatalf("akun = %d, ingin 2 (login + satu token)", len(f.Accounts))
	}
	login, tok := f.Accounts[0], f.Accounts[1]
	if login.ID != LoginID || login.Source != SourceLogin || login.Token != "" || login.Role != RoleUtama {
		t.Errorf("akun login = %+v", login)
	}
	if tok.ID != Fingerprint("github_pat_a") || tok.Source != SourceEnv || !tok.AddedAt.Equal(now) {
		t.Errorf("akun token = %+v", tok)
	}
}

func TestSaveLoadRoundTripPrivateFilesAndBackups(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o775); err != nil { // folder sudah ada dengan izin longgar
		t.Fatal(err)
	}
	path := PathIn(dir)
	if _, exists, err := Load(path); err != nil || exists {
		t.Fatalf("berkas belum ada: exists=%v err=%v", exists, err)
	}
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f := File{Strategy: StrategyBergiliran, Accounts: []Record{
		{Token: "", Role: RoleUtama},
		{Token: "github_pat_b", Role: "salah", Label: "Lab", Identity: Identity{Login: "mutia", ResetAt: &reset}},
	}}
	if err := Save(path, f, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("izin akun.json = %v (%v), ingin 0600", info.Mode().Perm(), err)
	}
	if dinfo, _ := os.Stat(dir); dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("izin folder = %v, ingin 0700", dinfo.Mode().Perm())
	}
	got, exists, err := Load(path)
	if err != nil || !exists {
		t.Fatalf("Load: %v %v", exists, err)
	}
	if got.Strategy != StrategyBergiliran || len(got.Accounts) != 2 || got.Accounts[1].Role != RoleUtama ||
		got.Accounts[1].ID != Fingerprint("github_pat_b") || got.Accounts[1].Login != "mutia" ||
		got.Accounts[1].ResetAt == nil || !got.Accounts[1].ResetAt.Equal(reset) || got.Accounts[0].Source != SourceLogin {
		t.Fatalf("hasil muat = %+v", got)
	}

	// Simpan berkali-kali dengan cadangan: hanya 20 cadangan terbaru yang disimpan, semuanya 0600.
	for i := 0; i < keepBackups+3; i++ {
		got.Accounts[1].Label = strings.Repeat("x", i)
		if err := Save(path, got, true); err != nil {
			t.Fatal(err)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "cadangan", "akun-*.json"))
	if len(backups) != keepBackups {
		t.Fatalf("cadangan = %d, ingin %d", len(backups), keepBackups)
	}
	for _, b := range backups {
		if bi, _ := os.Stat(b); bi.Mode().Perm() != 0o600 {
			t.Fatalf("izin cadangan %s = %v", b, bi.Mode().Perm())
		}
	}
	// Tanpa cadangan (cache identitas): jumlah cadangan tetap.
	if err := Save(path, got, false); err != nil {
		t.Fatal(err)
	}
	if again, _ := filepath.Glob(filepath.Join(dir, "cadangan", "akun-*.json")); len(again) != keepBackups {
		t.Fatalf("Save tanpa cadangan menambah berkas cadangan: %d", len(again))
	}
}

func TestLoadCorruptFileReportsWithoutContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(`{"accounts":[{"token":"github_pat_bocor"`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, exists, err := Load(path)
	if err == nil || !exists {
		t.Fatalf("berkas rusak harus galat: exists=%v err=%v", exists, err)
	}
	if strings.Contains(err.Error(), "bocor") {
		t.Fatalf("galat memuat isi berkas: %v", err)
	}
}

func TestRecordJSONFlattensIdentity(t *testing.T) {
	raw, _ := json.Marshal(Record{ID: "x", Identity: Identity{Login: "budi", Plan: "enterprise"}})
	if !strings.Contains(string(raw), `"login":"budi"`) || !strings.Contains(string(raw), `"plan":"enterprise"`) {
		t.Fatalf("identitas harus rata di JSON: %s", raw)
	}
}

func TestValidators(t *testing.T) {
	for _, v := range Roles {
		if !ValidRole(v) {
			t.Errorf("peran %q harus sah", v)
		}
	}
	for _, v := range Strategies {
		if !ValidStrategy(v) {
			t.Errorf("strategi %q harus sah", v)
		}
	}
	if ValidRole("admin") || ValidStrategy("acak") || PathIn("") != "" {
		t.Fatal("nilai asing harus ditolak")
	}
}
