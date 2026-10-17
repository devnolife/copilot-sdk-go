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
| `GET /v1/status` | Status murah: model, mode auth, dan keadaan tiap akun di `account_pool` (siap/hampir habis/cooldown, sisa kuota; tanpa token, tanpa menyalakan runtime) |
| `GET /v1/models` | `{"models":[...], "detail":[{id,name,multiplier}]}` |
| `POST /v1/generate` | Satu giliran, balasan teks utuh (`json_mode` didukung) |
| `POST /v1/stream` | Satu giliran, NDJSON `delta`/`reasoning`/`tool`/`done`/`error` |
| `POST /v1/agent` | Sesi agentik; tool dieksekusi pemanggil lewat callback HTTP |

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
| `TURNITIN_COPILOT_GITHUB_TOKENS` | — | Pool multi-akun, dipisah koma; urutan = prioritas; entri `login` = akun login tersimpan |
| `TURNITIN_COPILOT_QUOTA_MIN_PERCENT` | `5` | Akun dengan sisa kuota premium ≤ nilai ini hanya dipakai bila tidak ada akun lain; `0` = mati |
| `TURNITIN_COPILOT_QUOTA_CHECK_SEC` | `600` | Interval baca ulang kuota akun yang runtime-nya hidup; `0` = tidak dibaca |
| `TURNITIN_COPILOT_MODEL` | runtime default | Model utama |
| `TURNITIN_COPILOT_CHEAP_MODEL` / `_HEAVY_MODEL` / `_AGENT_MODEL` | — | Model per tier |
| `TURNITIN_COPILOT_MODEL_FALLBACKS` | — | Model cadangan bila semua akun sedang cooldown |
| `TURNITIN_COPILOT_MAX_CONCURRENCY` | `2` | Sesi paralel per akun |
| `TURNITIN_COPILOT_TIMEOUT` | `180` | Detik, untuk generate/stream |
| `TURNITIN_COPILOT_AGENT_TIMEOUT` | `300` | Detik, untuk sesi agentik. Juga menentukan batas tulis server HTTP (`WriteTimeout` = nilai ini + 2 menit) yang memotong **setiap** respons, termasuk aliran dengan `timeout_sec` lebih panjang. Setel ≥ timeout klien terpanjang − 120, mis. `480` untuk generate IDE Tugas core (540 dtk) |
| `TURNITIN_COPILOT_PROVIDER_*` | — | BYOK (mis. arahkan ke Ollama lokal) |

### Banyak akun dan pengalihan saat kuota hampir habis

Satu copilotd bisa memakai beberapa akun GitHub Copilot sekaligus
(`TURNITIN_COPILOT_GITHUB_TOKENS`, urutan = prioritas):

- **Sebelum limit.** Kuota tiap akun dibaca lewat `Account.GetQuota` (API eksperimental SDK) saat runtime-nya
  hidup dan tiap `QUOTA_CHECK_SEC`. Akun dengan sisa kuota premium ≤ `QUOTA_MIN_PERCENT` hanya dipakai bila tidak
  ada akun lain. Ini penting untuk akun yang mengizinkan kelebihan pemakaian: setelah kuotanya habis, akun seperti
  itu tetap jalan sebagai pemakaian berbayar tanpa galat, sehingga hanya ambang ini yang mencegahnya.
- **Saat limit.**
  - Rate limit membuat akun cooldown 5 menit; kuota habis membuatnya cooldown 1 jam.
  - Permintaan yang gagal sebelum ada teks terkirim diulang dengan **model yang sama di akun lain**.
  - `MODEL_FALLBACKS` baru dicoba bila semua akun sedang cooldown.
  - Permintaan managed tidak diulang.

Tambah akun:

1. Buat token untuk akun itu. Pakai PAT berbutir (`github_pat_…`, github.com/settings/personal-access-tokens)
   milik akun pribadi dengan izin akun **Copilot Requests**, atau token OAuth (`gho_…`) dari `gh auth login`.
   PAT klasik (`ghp_…`) ditolak Copilot CLI. Tiap akun perlu lisensi Copilot sendiri; ikuti ketentuan GitHub.
2. Jalankan `scripts/tambah-akun.sh`. Skrip meminta token tanpa menampilkannya, memeriksa login dan sisa kuotanya,
   menolak akun yang sudah ada, menyimpan cadangan `.env` ke `~/backups/copilotd-env/`, lalu menambahkannya. Pada
   penambahan pertama, akun login tersimpan dipertahankan sebagai `login` di urutan pertama.
3. Restart copilotd: `systemctl --user restart revisi-copilotd`.
4. Periksa hasilnya dengan `go run ./cmd/akun` (login dan sisa kuota tiap akun) atau `GET /v1/status`.

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
