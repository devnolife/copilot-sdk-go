// Command copilotd menjalankan GitHub Copilot sebagai backend HTTP.
//
// Paket SDK Copilot untuk Python mensyaratkan Python >= 3.11, sedangkan
// virtualenv backend riset masih 3.10 dan berisi torch/transformers berukuran
// besar. Alih-alih membangun ulang virtualenv itu, seluruh akses Copilot
// dipusatkan di service Go ini: backend Python memanggilnya lewat HTTP lokal.
//
// Endpoint (semua di bawah /v1, dengan alias tanpa versi untuk klien lama):
//
//	GET  /health        Cek hidup (tanpa auth, untuk probe/systemd)
//	GET  /v1/status     Status murah: akun, model, mode auth
//	GET  /v1/models     Daftar model yang bisa dipakai akun ini
//	POST /v1/generate   Satu giliran, balasan teks utuh (dukung json_mode)
//	POST /v1/stream     Satu giliran, balasan NDJSON streaming
//	POST /v1/agent      Sesi agentik; tool dieksekusi lewat callback HTTP
//
// Service hanya bind ke loopback secara default dan mewajibkan header
// X-API-Key bila COPILOTD_API_KEY di-set.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/devnolife/copilot-sdk-go/internal/api"
	"github.com/devnolife/copilot-sdk-go/internal/config"
	"github.com/devnolife/copilot-sdk-go/internal/runtime"
)

// shutdownGrace memberi waktu permintaan yang sedang jalan untuk selesai.
const shutdownGrace = 30 * time.Second

func main() {
	cfg := config.Load()
	addr := flag.String("addr", cfg.Addr, "alamat listen")
	flag.Parse()
	cfg.Addr = *addr

	svc := runtime.NewService(cfg)
	defer svc.Shutdown()

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.New(svc, cfg.APIKey).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Sesi agent bisa panjang; beri kelonggaran di atas timeout service.
		WriteTimeout: cfg.AgentTimeout + 2*time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("copilotd: shutting down…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	auth := "login tersimpan"
	if len(cfg.GitHubTokens) > 0 {
		auth = "pool token"
	} else if cfg.GitHubToken != "" {
		auth = "token tunggal"
	}
	log.Printf("copilotd listening on %s (auth: %s, akun: %d, konkurensi/akun: %d)",
		cfg.Addr, auth, len(cfg.Tokens()), cfg.MaxConcurrency)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
