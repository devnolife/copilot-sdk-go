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

`github-copilot-sdk` untuk Python mensyaratkan Python >= 3.11, sedangkan
virtualenv backend riset masih 3.10 dan berisi torch/transformers ~6.7 GB.
Alih-alih membangun ulang virtualenv itu, **seluruh akses Copilot dipusatkan di
service Go ini**; backend FastAPI hanya berbicara HTTP ke sini
(`app/services/copilot_sdk.py` sekarang murni klien HTTP).

```bash
go build -o bin/copilotd ./cmd/copilotd
COPILOTD_ADDR=127.0.0.1:8791 ./bin/copilotd
```

### Endpoint

| Endpoint | Keterangan |
| --- | --- |
| `GET /health` | Cek hidup, tanpa auth (untuk probe/systemd) |
| `GET /v1/status` | Status murah: jumlah akun, model, mode auth |
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

Event `done` membawa `calls[]` berisi jejak seluruh pemanggilan tool.

### Konfigurasi

Membaca variabel `TURNITIN_COPILOT_*` yang sama dengan backend, jadi satu
berkas `.env` bisa dipakai bersama.

| Variabel | Default | Keterangan |
| --- | --- | --- |
| `COPILOTD_ADDR` | `127.0.0.1:8791` | Alamat listen |
| `COPILOTD_API_KEY` | — | Bila diisi, wajib header `X-API-Key` |
| `TURNITIN_COPILOT_GITHUB_TOKEN` | — | Token tunggal; kosong = pakai login `copilot`/`gh` |
| `TURNITIN_COPILOT_GITHUB_TOKENS` | — | Pool multi-akun, dipisah koma |
| `TURNITIN_COPILOT_MODEL` | runtime default | Model utama |
| `TURNITIN_COPILOT_CHEAP_MODEL` / `_HEAVY_MODEL` / `_AGENT_MODEL` | — | Model per tier |
| `TURNITIN_COPILOT_MODEL_FALLBACKS` | — | Model cadangan saat kena rate limit |
| `TURNITIN_COPILOT_MAX_CONCURRENCY` | `2` | Sesi paralel per akun |
| `TURNITIN_COPILOT_TIMEOUT` | `180` | Detik, untuk generate/stream |
| `TURNITIN_COPILOT_AGENT_TIMEOUT` | `300` | Detik, untuk sesi agentik |
| `TURNITIN_COPILOT_PROVIDER_*` | — | BYOK (mis. arahkan ke Ollama lokal) |

Akun yang kena rate limit masuk cooldown 5 menit dan permintaan berikutnya
dialihkan ke akun lain; kalau `MODEL_FALLBACKS` diisi, model cadangan dicoba
sebelum menyerah.

### Deploy

```bash
# systemd (user unit)
cp ../core/deploy/systemd/revisi-copilotd.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now revisi-copilotd

# Docker
docker build -t copilotd .
docker run --rm -p 127.0.0.1:8791:8791 \
  -e TURNITIN_COPILOT_GITHUB_TOKEN=ghp_xxx -e COPILOTD_API_KEY=rahasia copilotd
```

Container tidak punya sesi login interaktif, jadi pakai token — atau mount
`~/.copilot` dari host secara read-only.

## Prasyarat

- [Copilot CLI](https://docs.github.com/copilot/concepts/agents/about-copilot-cli)
  terpasang & sudah login (`copilot` → `/login`). SDK men-spawn runtime CLI
  ini via JSON-RPC stdio.
- Go 1.24+.

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
