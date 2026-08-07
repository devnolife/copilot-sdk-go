// PoC Copilot SDK (Go) — mengukur latensi tiap fase supaya jelas
// bagian mana yang lambat: spawn runtime, create session, atau inferensi.
//
// Jalankan: go run . [prompt]
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

func main() {
	prompt := "Sebutkan 3 alasan kenapa agent runtime lebih lambat dari API LLM langsung. Jawab singkat."
	if len(os.Args) > 1 {
		prompt = strings.Join(os.Args[1:], " ")
	}

	ctx := context.Background()
	total := time.Now()

	// Fase 1: spawn runtime CLI (JSON-RPC via stdio) — ini biaya cold start.
	t := time.Now()
	client := copilot.NewClient(&copilot.ClientOptions{LogLevel: "error"})
	if err := client.Start(ctx); err != nil {
		log.Fatalf("start runtime: %v", err)
	}
	defer client.Stop()
	fmt.Printf("⏱  spawn runtime   : %s\n", time.Since(t).Round(time.Millisecond))

	// Fase 2: create session. Model bisa dipilih via env COPILOT_MODEL.
	t = time.Now()
	session, err := client.CreateSession(ctx, &copilot.SessionConfig{
		Model:     os.Getenv("COPILOT_MODEL"),
		Streaming: copilot.Bool(true),
	})
	if err != nil {
		log.Fatalf("create session: %v", err)
	}
	defer session.Disconnect()
	fmt.Printf("⏱  create session  : %s\n", time.Since(t).Round(time.Millisecond))

	// Fase 3+4: prompt yang sama dua kali — dingin vs hangat (session reuse).
	for i, label := range []string{"prompt #1 (dingin)", "prompt #2 (hangat)"} {
		if err := run(ctx, session, prompt, label); err != nil {
			log.Fatalf("send: %v", err)
		}
		if i == 0 {
			fmt.Println(strings.Repeat("─", 60))
		}
	}

	fmt.Printf("\n⏱  TOTAL           : %s\n", time.Since(total).Round(time.Millisecond))
}

func run(ctx context.Context, session *copilot.Session, prompt, label string) error {
	done := make(chan struct{})
	var firstToken time.Duration
	var answer string
	start := time.Now()

	unsubscribe := session.On(func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			if firstToken == 0 {
				firstToken = time.Since(start)
			}
		case *copilot.AssistantMessageData:
			answer = d.Content
		case *copilot.SessionIdleData:
			close(done)
		}
	})
	defer unsubscribe()

	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
		return err
	}

	select {
	case <-done:
	case <-time.After(180 * time.Second):
		return fmt.Errorf("timeout 180s menunggu jawaban")
	}

	fmt.Printf("⏱  %s\n", label)
	fmt.Printf("   token pertama   : %s\n", firstToken.Round(time.Millisecond))
	fmt.Printf("   selesai         : %s\n", time.Since(start).Round(time.Millisecond))
	fmt.Printf("   jawaban         : %s\n", truncate(answer, 300))
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
