# copilot-sdk-go

PoC Copilot SDK (Go) — mengukur latensi tiap fase runtime Copilot CLI:
spawn runtime, create session, dan inferensi (dingin vs hangat).

```bash
go run . "prompt anda"              # benchmark latensi
go run ./cmd/models                 # daftar model tersedia
go run ./cmd/websearch "kueri"      # contoh pencarian web
```

Model bisa dipilih via env `COPILOT_MODEL`.

## copilot-cli — pakai Copilot dari terminal

Binary siap pakai untuk memakai langganan Copilot akun sendiri.

```bash
go build -o bin/copilot-cli ./cmd/copilot-cli
install -m 755 bin/copilot-cli ~/.local/bin/

copilot-cli whoami                          # cek auth + runtime
copilot-cli models                          # daftar model (--json untuk mesin)
copilot-cli ask "jelaskan apa itu SLR"      # tanya sekali jalan
copilot-cli ask "ringkas ini:" < paper.txt  # baca stdin
copilot-cli ask --web "rilis Go terbaru?"   # izinkan pencarian web
copilot-cli chat --model claude-opus-5      # sesi interaktif
copilot-cli serve --addr 127.0.0.1:8791     # jalankan copilotd
```

Autentikasi otomatis: token eksplisit (`--token`, atau env
`COPILOT_GITHUB_TOKEN`/`GITHUB_TOKEN`/`GH_TOKEN`) dipakai lebih dulu; kalau
tidak ada, runtime memakai kredensial `copilot`/`gh` yang sudah login.

Runtime CLI dicari lewat `COPILOT_CLI_PATH` (atau `TURNITIN_COPILOT_CLI_PATH`),
lalu `copilot` di `PATH`. Kalau di mesinmu ada beberapa binary bernama
`copilot` (mis. wrapper bawaan editor), tunjuk yang benar secara eksplisit:

```bash
export COPILOT_CLI_PATH="$(npm root -g)/../bin/copilot"
```

Semua tool bawaan runtime (shell, edit file, fetch) dimatikan secara default —
hanya `--web` yang membuka `web_search` + `web_fetch`.

## copilotd — Copilot sebagai backend HTTP

Awalnya lahir karena `github-copilot-sdk` untuk Python mensyaratkan Python >= 3.11
sedangkan virtualenv backend riset waktu itu masih 3.10 (berisi torch/transformers
~6.7 GB). Kini venv backend sudah 3.11, tetapi **seluruh akses Copilot tetap
dipusatkan di service Go ini**: pool multi-akun, cooldown rate-limit, failover
model, dan BYOK dikelola di satu proses, dan backend Python tidak perlu memasang
SDK apa pun — backend FastAPI hanya berbicara HTTP ke sini
(`app/services/copilot_sdk.py` murni klien HTTP).

```bash
go build -o bin/copilotd ./cmd/copilotd
COPILOTD_ADDR=127.0.0.1:8791 ./bin/copilotd
```

### Endpoint

| Endpoint | Keterangan |
| --- | --- |
| `GET /health` | Cek hidup, tanpa auth (untuk probe/systemd) |
| `GET /v1/status` | Status murah: model, mode auth, strategi pool, dan keadaan tiap akun di `account_pool` (siap/sibuk/hampir habis/cooldown/nonaktif, sisa kuota; tanpa token, tanpa menyalakan runtime) |
| `GET /v1/models` | `{"models":[...], "detail":[{id,name,multiplier}]}` |
| `POST /v1/generate` | Satu giliran, balasan teks utuh (`json_mode` didukung) |
| `POST /v1/stream` | Satu giliran, NDJSON `delta`/`reasoning`/`tool`/`done`/`error` |
| `POST /v1/agent` | Sesi agentik; tool dieksekusi pemanggil lewat callback HTTP |
| `GET /v1/accounts?days=30` | Pool akun untuk admin: peran, status, kuota, tanggal reset, perkiraan habis, pemakaian per hari |
| `POST /v1/accounts` | Tambah akun `{token, role, label, actor}`; token diperiksa dulu di runtime sementara |
| `PATCH /v1/accounts/{id}` · `DELETE /v1/accounts/{id}` | Ubah peran/label · keluarkan akun (akun aktif terakhir tidak bisa) |
| `POST /v1/accounts/refresh` · `PUT /v1/accounts/strategy` | Baca ulang identitas & kuota sekarang · ganti strategi pembagian |

`/models` dan `/stream` tanpa prefiks `/v1` tetap dilayani sebagai alias.

### Sesi agentik

Tool tidak dieksekusi di Go — database, model ML, dan konteks pengguna ada di
backend Python. copilotd memanggil balik `callback_url` setiap kali model butuh
sebuah tool:

```jsonc
// permintaan
POST /v1/agent
{
  "prompt": "...",
  "tools": [{"name": "cari_paper", "description": "...", "parameters": {…}}],
  "callback_url": "http://127.0.0.1:8765/api/v1/internal/copilot-tool",
  "callback_token": "<acak, sekali pakai>",
  "callback_headers": {"X-API-Key": "<kunci backend>"}
}

// callback ke pemanggil
POST <callback_url>  {"token": "...", "tool": "cari_paper", "arguments": {…}}
// balasan yang diharapkan
{"result": "..."}      // atau {"error": "..."}
```

Event `done` membawa `calls[]` berisi jejak seluruh pemanggilan tool, serta
`usage[]` — satu entri per panggilan API model (token input/output/cache,
reasoning, cost, nano AIU, durasi) dari event `assistant.usage` SDK.
`POST /v1/generate` meneruskan `usage[]` yang sama di balasan JSON-nya.

### Managed generation

Core admission dapat mengirim `managed: true` beserta `max_ai_credits`,
`max_output_tokens`, `max_tool_calls` dan `timeout_sec` eksplisit.
`GET /v1/status` mengiklankan `managed_generation_v1`.
Mode ini menonaktifkan fallback model internal, custom instructions/file
hooks serta tool penjadwal/coauthor ambient; setiap tool melewati hook
batas pemakaian. Timeout mencoba `Session.Abort`; abort yang tidak dapat
dikonfirmasi dilaporkan `generation_result_unknown`, bukan dianggap selesai.

SDK dipin dan request meneruskan `SessionLimits.MaxAiCredits` serta
capability output token yang didukung versi itu. Batas kredit/runtime
tidak sama dengan billing token aktual atau bukti satu sesi = satu
inferensi. Dukungan runtime/provider dan perilaku biaya tetap perlu
diukur dalam benchmark yang disetujui; unit test tidak menjalankan model.
Permintaan lama tanpa `managed` mempertahankan perilaku sebelumnya.

### Konfigurasi

Membaca variabel `TURNITIN_COPILOT_*` yang sama dengan backend, jadi satu
berkas `.env` bisa dipakai bersama.

| Variabel | Default | Keterangan |
| --- | --- | --- |
| `COPILOTD_ADDR` | `127.0.0.1:8791` | Alamat listen |
| `COPILOTD_API_KEY` | — | Bila diisi, wajib header `X-API-Key` |
| `TURNITIN_COPILOT_GITHUB_TOKEN` | — | Token tunggal; kosong = pakai login `copilot`/`gh` |
| `TURNITIN_COPILOT_GITHUB_TOKENS` | — | Pool multi-akun dari `.env` (dipisah koma; entri `login` = akun login tersimpan); diabaikan setelah `akun.json` ada |
| `TURNITIN_COPILOT_STATE_DIR` | `~/.local/state/copilotd` | Tempat `akun.json` (daftar akun, 0600) dan `pemakaian.json`; kosong = tanpa berkas |
| `TURNITIN_COPILOT_POOL_STRATEGY` | `proporsional` | Strategi awal (`proporsional`/`bergiliran`/`prioritas`) selama `akun.json` belum menyimpan strategi |
| `TURNITIN_COPILOT_TIMEZONE` | `Asia/Makassar` | Batas hari catatan pemakaian harian |
| `TURNITIN_COPILOT_QUOTA_MIN_PERCENT` | `5` | Akun dengan sisa kuota premium ≤ nilai ini hanya dipakai bila tidak ada akun sehat; `0` = mati |
| `TURNITIN_COPILOT_QUOTA_CHECK_SEC` | `600` | Interval baca ulang kuota (tanpa menyalakan runtime baru); `0` = tidak dibaca |
| `TURNITIN_COPILOT_MODEL` | runtime default | Model utama: dipakai permintaan tanpa nama model, termasuk tier utama mode Otomatis core. Alias Intelligent di core diatur terpisah (`COPILOT_MODEL` core) |
| `TURNITIN_COPILOT_CHEAP_MODEL` | model utama | Tier `cheap` tanpa nama model. Core mengirim nama model hemat sendiri (`COPILOT_CHEAP_MODEL` core); samakan nilainya |
| `TURNITIN_COPILOT_HEAVY_MODEL` | model utama | Tier `heavy` tanpa nama model: langkah menulis berkas IDE Tugas core |
| `TURNITIN_COPILOT_AGENT_MODEL` | model utama | Sesi `/v1/agent` (agen bertool) tanpa nama model; kosong = ikut model utama |
| `TURNITIN_COPILOT_MODEL_FALLBACKS` | — | Model cadangan, dicoba berurutan bila model tier gagal dengan galat yang layak diulang di semua akun. Hanya untuk permintaan tanpa nama model |
| `TURNITIN_COPILOT_MAX_CONCURRENCY` | `2` | Sesi paralel per akun |
| `TURNITIN_COPILOT_TIMEOUT` | `180` | Detik, untuk generate/stream |
| `TURNITIN_COPILOT_AGENT_TIMEOUT` | `300` | Detik, untuk sesi agentik. Juga menentukan batas tulis server HTTP (`WriteTimeout` = nilai ini + 2 menit) yang memotong **setiap** respons, termasuk aliran dengan `timeout_sec` lebih panjang. Setel ≥ timeout klien terpanjang − 120, mis. `480` untuk generate IDE Tugas core (540 dtk) |
| `TURNITIN_COPILOT_PROVIDER_*` | — | BYOK (mis. arahkan ke Ollama lokal) |

### Banyak akun: pembagian, pengalihan, dan admin

Satu copilotd memakai beberapa akun GitHub Copilot sekaligus. Daftar akun disimpan di
`$TURNITIN_COPILOT_STATE_DIR/akun.json` (bawaan `~/.local/state/copilotd`, berkas 0600, cadangan tiap perubahan di
`cadangan/`). Selama berkas itu belum ada, `TURNITIN_COPILOT_GITHUB_TOKENS` di `.env` yang dipakai. Bila berkas ada
tetapi rusak, copilotd tetap jalan dengan akun `.env` dan menolak semua perubahan akun (409, `store_error` di
`/v1/accounts`) sampai berkas diperbaiki atau dipulihkan dari `cadangan/`, supaya akun yang hanya ada di berkas itu
tidak tertimpa.

- **Pembagian.** Tiap akun punya peran `utama`, `cadangan`, atau `nonaktif`. Permintaan dibagi ke akun utama
  dengan *smooth weighted round-robin* menurut strategi:
  - `proporsional` (bawaan): bobot = sisa unit kuota, jadi akun yang sisanya besar melayani lebih banyak dan
    semua akun habis kira-kira bersamaan;
  - `bergiliran`: bobot sama;
  - `prioritas`: urutan daftar.

  Urutan kelas: utama sehat → cadangan sehat (bila utama penuh atau tidak tersedia) → akun hampir habis (sisa
  terbesar dulu) → akun cooldown yang paling cepat pulih. Kapasitas paralel = jumlah akun × `MAX_CONCURRENCY`.
- **Sebelum limit.** Kuota dibaca lewat `Account.GetQuota` (API eksperimental SDK): saat runtime akun start, tiap
  `QUOTA_CHECK_SEC`, dan untuk akun bertoken yang runtime-nya belum hidup lewat runtime akun lain
  (`GetQuota{GitHubToken}`). Akun dengan sisa kuota premium ≤ `QUOTA_MIN_PERCENT` hanya dipakai bila tidak ada
  akun sehat. Ini penting untuk akun yang mengizinkan kelebihan pemakaian: setelah kuotanya habis, akun seperti
  itu tetap jalan sebagai pemakaian berbayar tanpa galat, sehingga hanya ambang ini yang mencegahnya.
- **Tanggal reset.** Diambil dari `Account.GetCurrentAuth` (`quota_reset_date_utc`), bukan dari `resetDate`
  snapshot GetQuota (isinya cap waktu snapshot).
- **Saat limit.**
  - Rate limit membuat akun cooldown 5 menit, kuota habis 1 jam, token ditolak (errorType
    `authentication`/`authorization`) 15 menit.
  - Permintaan yang gagal sebelum ada teks terkirim diulang dengan **model yang sama di akun lain**.
  - Daftar model bisa berbeda antar-akun (seat dari organisasi berbeda). Akun yang menolak sebuah model
    (`Model "x" is not available`) dilewati untuk model itu selama 1 jam (`models_unavailable` di
    `/v1/accounts`), dan permintaannya langsung diulang di akun lain.
  - `MODEL_FALLBACKS` baru dicoba bila semua akun sedang cooldown.
  - Permintaan managed tidak diulang.
- **Pemakaian.** Tiap permintaan dicatat ke akun yang melayani: jumlah, berhasil/gagal, token, `cost`,
  `nano_aiu`, dan durasi, per hari WITA. Data ini disimpan di `pemakaian.json` (90 hari, tanpa token), bersama
  riwayat kuota (45 hari) untuk perkiraan kapan ambang dan kuota habis.

Kelola akun:

- **Admin revisi.studio → Akun Copilot.** Tambah akun dengan menempel token, ubah peran/label, hapus, ganti
  strategi, dan segarkan kuota, semuanya tanpa restart. Core meneruskan aksi ini ke `/v1/accounts` dan mencatatnya
  di log audit admin.
- **Terminal:** `scripts/tambah-akun.sh [--peran cadangan] [--label teks]`. Token diminta tanpa tampil. Bila
  copilotd jalan, akun langsung aktif lewat API; bila tidak, ditulis ke `akun.json`. `go run ./cmd/akun`
  menampilkan peran, login, status, sisa kuota, reset, dan pemakaian 7 hari.
- **Pindah dari `.env`:** `go run ./cmd/akun migrasi` membuat `akun.json` dari `.env`, menyimpan cadangan `.env`
  ke `~/backups/copilotd-env/`, lalu mengganti baris token di `.env` dengan komentar. Setelah itu restart
  copilotd.

Token yang diterima: PAT berbutir (`github_pat_…`) milik akun pribadi dengan izin akun **Copilot Requests**, token
OAuth (`gho_…`), atau token pengguna GitHub App (`ghu_…`). PAT klasik (`ghp_…`) ditolak Copilot CLI. Tiap akun
perlu lisensi Copilot sendiri; ikuti ketentuan GitHub dan organisasi pemberi seat.

### Deploy

```bash
# systemd (user unit)
cp ../core/deploy/systemd/revisi-copilotd.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now revisi-copilotd

# Docker
docker build -t copilotd .
docker run --rm -p 127.0.0.1:8791:8791 \
  -e TURNITIN_COPILOT_GITHUB_TOKEN=github_pat_xxx -e COPILOTD_API_KEY=rahasia copilotd
```

Container tidak punya sesi login interaktif, jadi pakai token — atau mount
`~/.copilot` dari host secara read-only.

## Prasyarat

- [Copilot CLI](https://docs.github.com/copilot/concepts/agents/about-copilot-cli)
  terpasang & sudah login (`copilot` → `/login`). SDK men-spawn runtime CLI
  ini via JSON-RPC stdio.
- Go 1.24+.

## Pengembangan

```bash
go vet ./... && go test -race ./...   # suite unit: config, pool, session, tools, api
go test -cover ./internal/...         # cakupan per paket
```

Suite berjalan tanpa Copilot CLI: pool dan service bersifat lazy, jadi test
hanya menyentuh jalur validasi, pemilihan akun/cooldown, penyusunan config
sesi, dan callback tool (lewat `httptest`). Jalur yang benar-benar men-spawn
runtime (`Stream`, `Models`) sengaja tidak diuji di sini. CI (GitHub Actions)
menjalankan gofmt, vet, build, dan test pada setiap push/PR.

## Cara membuat Copilot melakukan pencarian web

Runtime Copilot CLI sudah punya tool web bawaan (`web_search` untuk mencari,
`web_fetch` untuk mengambil isi URL). Dari SDK, ada **dua kunci** supaya tool
itu benar-benar jalan:

**1. Tangani permintaan izin.** Tool berefek keluar (termasuk akses web) minta
persetujuan dulu. Tanpa handler, permintaan izinnya menggantung dan agent
tidak jadi mencari. Daftarkan `OnPermissionRequest` saat membuat session:

```go
import (
    copilot "github.com/github/copilot-sdk/go"
    "github.com/github/copilot-sdk/go/rpc"
)

session, err := client.CreateSession(ctx, &copilot.SessionConfig{
    OnPermissionRequest: func(req copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
        // Produksi: inspeksi req dulu; jangan auto-setujui perintah shell.
        return &rpc.PermissionDecisionApproveOnce{}, nil
    },
})
```

**2. Minta eksplisit di prompt.** Model memutuskan sendiri kapan memakai
tool. Frasa seperti *"Cari di web: …"* / *"Search the web for …"* membuat
model memilih `web_search`; kalau diberi URL spesifik ia memakai `web_fetch`.

Pantau tool yang dipakai lewat event `ToolExecutionStartData`:

```go
session.On(func(event copilot.SessionEvent) {
    if d, ok := event.Data.(*copilot.ToolExecutionStartData); ok {
        fmt.Println("tool:", d.ToolName) // web_search / web_fetch / ...
    }
})
```

Contoh lengkap yang bisa langsung dijalankan: [`cmd/websearch`](cmd/websearch/main.go).
Contoh keluaran:

```
$ go run ./cmd/websearch "kapan Go 1.25 dirilis"
🔧 tool: web_fetch
────────────────────────────────────────────────────────────
**Go 1.25.0 dirilis pada 12 Agustus 2025.**
Sumber: go.dev/doc/devel/release
```

Catatan:

- `SessionConfig.AvailableTools` / `ExcludedTools` bisa dipakai untuk
  membatasi tool (mis. hanya izinkan `web_search` + `web_fetch`).
- Ketersediaan web search mengikuti kebijakan/entitlement akun Copilot dan
  versi CLI yang terpasang; kalau tool tidak tersedia, model akan menjawab
  dari pengetahuan internal tanpa error.
