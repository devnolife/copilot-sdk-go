package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	copilot "github.com/github/copilot-sdk/go"
)

// streamTurn mengirim satu prompt dan mencetak jawabannya sambil mengalir.
// Mengembalikan teks final utuh.
func streamTurn(ctx context.Context, session *copilot.Session, prompt string, raw bool) (string, error) {
	var (
		mu       sync.Mutex
		final    string
		failure  string
		streamed bool
		once     sync.Once
	)
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }

	unsubscribe := session.On(func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			if d.DeltaContent == "" {
				return
			}
			mu.Lock()
			streamed = true
			mu.Unlock()
			fmt.Print(d.DeltaContent)
		case *copilot.ToolExecutionStartData:
			// Tanpa penanda ini, jeda saat pencarian web terlihat seperti hang.
			fmt.Fprintf(os.Stderr, "\n[%s…]\n", d.ToolName)
		case *copilot.AssistantMessageData:
			mu.Lock()
			final = d.Content
			mu.Unlock()
		case *copilot.SessionErrorData:
			mu.Lock()
			failure = d.Message
			mu.Unlock()
			finish()
		case *copilot.SessionIdleData:
			finish()
		}
	})
	defer unsubscribe()

	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
		return "", fmt.Errorf("gagal mengirim prompt: %w", err)
	}

	select {
	case <-done:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	mu.Lock()
	text, hadStream, errMsg := final, streamed, failure
	mu.Unlock()

	if errMsg != "" {
		return "", fmt.Errorf("copilot: %s", errMsg)
	}
	// Sebagian model tidak mengirim delta sama sekali; cetak hasil finalnya.
	if !hadStream && text != "" {
		fmt.Print(text)
	}
	if !raw {
		fmt.Println()
	}
	return text, nil
}

func printJSON(payload any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// runServe menjalankan copilotd. Binary-nya dicari di direktori yang sama
// dengan copilot-cli, lalu di PATH.
func runServe(args []string) {
	candidates := []string{}
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), "copilotd"))
	}
	if found, err := exec.LookPath("copilotd"); err == nil {
		candidates = append(candidates, found)
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue
		}
		cmd := exec.Command(path, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintf(os.Stderr,
		"error: binary copilotd tidak ditemukan (dicari di: %s)\n"+
			"Build dulu: go build -o bin/copilotd ./cmd/copilotd\n",
		strings.Join(candidates, ", "))
	os.Exit(1)
}
