// Package main mengimplementasikan copilot-cli — antarmuka baris perintah
// untuk memakai langganan GitHub Copilot pribadi dari terminal.
//
// Perintah:
//
//	copilot-cli whoami            Cek autentikasi & runtime
//	copilot-cli models            Daftar model yang bisa dipakai akun ini
//	copilot-cli ask "pertanyaan"  Tanya sekali jalan (bisa baca stdin)
//	copilot-cli chat              Sesi percakapan interaktif
//	copilot-cli serve             Jalankan copilotd (server HTTP lokal)
//
// Autentikasi otomatis: pakai token eksplisit bila ada (flag --token atau env
// COPILOT_GITHUB_TOKEN / GITHUB_TOKEN / GH_TOKEN), selain itu memakai
// kredensial `copilot`/`gh` yang sudah login di mesin ini.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

const usage = `copilot-cli — pakai langganan GitHub Copilot dari terminal.

Penggunaan:
  copilot-cli <perintah> [opsi] [argumen]

Perintah:
  whoami            Cek autentikasi dan runtime Copilot
  models            Daftar model yang tersedia untuk akun ini
  ask <pertanyaan>  Tanya sekali jalan; tanpa argumen membaca stdin
  chat              Sesi percakapan interaktif
  serve             Jalankan server HTTP lokal (copilotd)

Opsi umum:
  --model <id>      Model yang dipakai (default: env COPILOT_MODEL)
  --system <teks>   System prompt kustom
  --token <token>   Token GitHub; default memakai login yang sudah ada
  --web             Izinkan model mencari di web (ask/chat)
  --raw             Cetak jawaban apa adanya tanpa hiasan
  --json            Keluaran JSON (models)

Contoh:
  copilot-cli models
  copilot-cli ask "ringkas paper ini" < paper.txt
  copilot-cli ask --web "rilis Go terbaru apa?"
  copilot-cli chat --model claude-opus-5
`

type options struct {
	model  string
	system string
	token  string
	web    bool
	raw    bool
	asJSON bool
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	command := os.Args[1]
	switch command {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	case "serve":
		// Delegasikan ke copilotd; argumennya diteruskan apa adanya.
		runServe(os.Args[2:])
		return
	}

	fs := flag.NewFlagSet(command, flag.ExitOnError)
	var opts options
	fs.StringVar(&opts.model, "model", os.Getenv("COPILOT_MODEL"), "model yang dipakai")
	fs.StringVar(&opts.system, "system", "", "system prompt kustom")
	fs.StringVar(&opts.token, "token", "", "token GitHub (default: login yang sudah ada)")
	fs.BoolVar(&opts.web, "web", false, "izinkan pencarian web")
	fs.BoolVar(&opts.raw, "raw", false, "cetak jawaban tanpa hiasan")
	fs.BoolVar(&opts.asJSON, "json", false, "keluaran JSON")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch command {
	case "whoami":
		err = runWhoami(ctx, opts)
	case "models":
		err = runModels(ctx, opts)
	case "ask":
		err = runAsk(ctx, opts, strings.Join(fs.Args(), " "))
	case "chat":
		err = runChat(ctx, opts)
	default:
		fmt.Fprintf(os.Stderr, "perintah tidak dikenal: %s\n\n%s", command, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// resolveToken mencari token eksplisit dari flag lalu environment. String
// kosong berarti "pakai kredensial copilot/gh yang sudah login".
func resolveToken(flagToken string) string {
	if strings.TrimSpace(flagToken) != "" {
		return strings.TrimSpace(flagToken)
	}
	for _, name := range []string{"COPILOT_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// resolveCLIPath mencari binary runtime Copilot CLI. Runtime bawaan SDK tidak
// selalu ada di mesin ini, jadi path eksplisit (env yang sama dengan copilotd)
// dipakai lebih dulu, lalu `copilot` di PATH.
func resolveCLIPath() string {
	for _, name := range []string{"TURNITIN_COPILOT_CLI_PATH", "COPILOT_CLI_PATH"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	if found, err := exec.LookPath("copilot"); err == nil {
		return found
	}
	return ""
}

// newClient menyalakan runtime Copilot. Token eksplisit menang; kalau tidak
// ada, runtime memakai OAuth `copilot`/`gh` yang tersimpan.
func newClient(ctx context.Context, opts options) (*copilot.Client, error) {
	clientOpts := &copilot.ClientOptions{LogLevel: "error"}
	if token := resolveToken(opts.token); token != "" {
		clientOpts.GitHubToken = token
	} else {
		clientOpts.UseLoggedInUser = copilot.Bool(true)
	}
	if path := resolveCLIPath(); path != "" {
		clientOpts.Connection = copilot.StdioConnection{Path: path}
	}
	client := copilot.NewClient(clientOpts)
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf(
			"runtime Copilot gagal start: %w\n"+
				"Pastikan Copilot CLI terpasang & sudah login, atau tunjuk binary-nya:\n"+
				"  export COPILOT_CLI_PATH=/path/ke/copilot",
			err)
	}
	return client, nil
}

func runWhoami(ctx context.Context, opts options) error {
	source := "login tersimpan (copilot/gh)"
	if resolveToken(opts.token) != "" {
		source = "token eksplisit (flag/env)"
	}
	fmt.Printf("Autentikasi : %s\n", source)

	client, err := newClient(ctx, opts)
	if err != nil {
		fmt.Println("Runtime     : gagal")
		return err
	}
	defer client.Stop()

	models, err := client.ListModels(ctx)
	if err != nil {
		fmt.Println("Runtime     : jalan, tapi daftar model ditolak")
		return err
	}
	fmt.Printf("Runtime     : jalan\n")
	fmt.Printf("Model aktif : %d tersedia\n", len(models))
	if opts.model != "" {
		fmt.Printf("Model default: %s\n", opts.model)
	}
	return nil
}

func runModels(ctx context.Context, opts options) error {
	client, err := newClient(ctx, opts)
	if err != nil {
		return err
	}
	defer client.Stop()

	models, err := client.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("gagal mengambil daftar model: %w", err)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })

	if opts.asJSON {
		return printJSON(models)
	}
	for _, m := range models {
		multiplier := ""
		if m.Billing != nil && m.Billing.Multiplier != nil {
			multiplier = fmt.Sprintf("  ×%.2f", *m.Billing.Multiplier)
		}
		fmt.Printf("%-28s %s%s\n", m.ID, m.Name, multiplier)
	}
	return nil
}

func runAsk(ctx context.Context, opts options, prompt string) error {
	prompt, err := promptWithStdin(prompt)
	if err != nil {
		return err
	}
	if prompt == "" {
		return errors.New("pertanyaan kosong; beri argumen atau pipe lewat stdin")
	}

	client, err := newClient(ctx, opts)
	if err != nil {
		return err
	}
	defer client.Stop()

	session, err := newSession(ctx, client, opts)
	if err != nil {
		return err
	}
	defer session.Disconnect()

	_, err = streamTurn(ctx, session, prompt, opts.raw)
	return err
}

func runChat(ctx context.Context, opts options) error {
	client, err := newClient(ctx, opts)
	if err != nil {
		return err
	}
	defer client.Stop()

	session, err := newSession(ctx, client, opts)
	if err != nil {
		return err
	}
	defer session.Disconnect()

	model := opts.model
	if model == "" {
		model = "default"
	}
	fmt.Printf("Copilot chat (model: %s). Ketik /keluar untuk selesai.\n\n", model)

	reader := bufio.NewScanner(os.Stdin)
	reader.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("kamu › ")
		if !reader.Scan() {
			fmt.Println()
			return reader.Err()
		}
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}
		if line == "/keluar" || line == "/exit" || line == "/quit" {
			return nil
		}

		fmt.Print("\ncopilot › ")
		if _, err := streamTurn(ctx, session, line, true); err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		}
		fmt.Println()
	}
}

// promptWithStdin menggabungkan argumen dengan isi stdin bila di-pipe.
func promptWithStdin(prompt string) (string, error) {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return strings.TrimSpace(prompt), nil
	}
	// Mode char device = terminal interaktif, tidak ada yang di-pipe.
	if stat.Mode()&os.ModeCharDevice != 0 {
		return strings.TrimSpace(prompt), nil
	}
	piped, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("gagal membaca stdin: %w", err)
	}
	text := strings.TrimSpace(string(piped))
	if text == "" {
		return strings.TrimSpace(prompt), nil
	}
	if strings.TrimSpace(prompt) == "" {
		return text, nil
	}
	return strings.TrimSpace(prompt) + "\n\n" + text, nil
}

// noTools adalah nama tool yang pasti tidak ada di registry runtime.
// Mengirimnya sebagai satu-satunya AvailableTools mengunci semua tool bawaan
// (shell, edit, fetch) supaya CLI ini tidak bisa menyentuh mesin pengguna.
const noTools = "__none__"

var webTools = []string{"web_search", "web_fetch"}

func newSession(ctx context.Context, client *copilot.Client, opts options) (*copilot.Session, error) {
	cfg := &copilot.SessionConfig{
		Model:     opts.model,
		Streaming: copilot.Bool(true),
	}
	if opts.web {
		cfg.AvailableTools = webTools
		cfg.OnPermissionRequest = func(copilot.PermissionRequest, copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}
	} else {
		cfg.AvailableTools = []string{noTools}
	}
	if opts.system != "" {
		cfg.SystemMessage = &copilot.SystemMessageConfig{Mode: "replace", Content: opts.system}
	}
	session, err := client.CreateSession(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("gagal membuat sesi: %w", err)
	}
	return session, nil
}
