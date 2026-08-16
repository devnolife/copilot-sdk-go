# copilotd — GitHub Copilot sebagai backend HTTP.
#
# Build:
#   docker build -t copilotd .
#
# Jalankan (autentikasi lewat token, bukan login interaktif — container tidak
# punya sesi `copilot login`):
#   docker run --rm -p 127.0.0.1:8791:8791 \
#     -e TURNITIN_COPILOT_GITHUB_TOKEN=ghp_xxx \
#     -e COPILOTD_ADDR=0.0.0.0:8791 \
#     -e COPILOTD_API_KEY=rahasia \
#     copilotd
#
# Alternatif: mount kredensial CLI dari host dengan
#   -v "$HOME/.copilot:/home/app/.copilot:ro"

# ── Tahap build ────────────────────────────────────────────────────
FROM golang:1.24-bookworm AS build

WORKDIR /src

# Unduh dependensi lebih dulu agar layer-nya bisa di-cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO dimatikan supaya binary-nya statis dan bisa jalan di image ramping.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/copilotd ./cmd/copilotd

# ── Tahap runtime ──────────────────────────────────────────────────
# Runtime Copilot yang di-embed SDK butuh glibc + sertifikat TLS, jadi image
# distroless/alpine tidak cukup.
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# Jalankan sebagai non-root.
RUN useradd --create-home --uid 10001 app
USER app
WORKDIR /home/app

COPY --from=build /out/copilotd /usr/local/bin/copilotd

ENV COPILOTD_ADDR=0.0.0.0:8791 \
    HOME=/home/app

EXPOSE 8791

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${COPILOTD_ADDR##*:}/health" || exit 1

ENTRYPOINT ["copilotd"]
