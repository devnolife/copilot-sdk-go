# copilot-sdk-go

PoC Copilot SDK (Go) — mengukur latensi tiap fase runtime Copilot CLI:
spawn runtime, create session, dan inferensi (dingin vs hangat).

```bash
go run . "prompt anda"              # benchmark latensi
go run ./cmd/models                 # daftar model tersedia
go run ./cmd/websearch "kueri"      # contoh pencarian web
```

Model bisa dipilih via env `COPILOT_MODEL`.

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
