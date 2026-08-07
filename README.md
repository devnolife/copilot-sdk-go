# copilot-sdk-go

PoC Copilot SDK (Go) — mengukur latensi tiap fase runtime Copilot CLI:
spawn runtime, create session, dan inferensi (dingin vs hangat).

```bash
go run . "prompt anda"          # benchmark latensi
go run ./cmd/models             # daftar model tersedia
```

Model bisa dipilih via env `COPILOT_MODEL`.
