// Contoh: menyuruh Copilot melakukan PENCARIAN WEB dari Go.
//
// Kuncinya dua hal:
//  1. Izin — tool web_search butuh persetujuan. Tanpa OnPermissionRequest,
//     permintaan izin menggantung dan agent tidak jadi mencari.
//  2. Prompt — minta eksplisit "cari di web" supaya model memilih tool
//     web_search (tersedia built-in di runtime Copilot CLI).
//
// Jalankan: go run ./cmd/websearch "kueri anda"
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func main() {
	query := "berita terbaru rilis bahasa Go"
	if len(os.Args) > 1 {
		query = strings.Join(os.Args[1:], " ")
	}

	ctx := context.Background()
	client := copilot.NewClient(&copilot.ClientOptions{LogLevel: "error"})
	if err := client.Start(ctx); err != nil {
		log.Fatalf("start runtime: %v", err)
	}
	defer client.Stop()

	session, err := client.CreateSession(ctx, &copilot.SessionConfig{
		Model:     os.Getenv("COPILOT_MODEL"),
		Streaming: copilot.Bool(true),
		// Auto-setujui permintaan izin tool. Di aplikasi nyata, periksa
		// request-nya dulu (jangan setujui buta perintah shell dsb.).
		OnPermissionRequest: func(req copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		},
	})
	if err != nil {
		log.Fatalf("create session: %v", err)
	}
	defer session.Disconnect()

	done := make(chan struct{})
	var answer string

	unsubscribe := session.On(func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.ToolExecutionStartData:
			fmt.Printf("🔧 tool: %s\n", d.ToolName)
		case *copilot.AssistantMessageData:
			answer = d.Content
		case *copilot.SessionIdleData:
			close(done)
		}
	})
	defer unsubscribe()

	prompt := "Cari di web: " + query + ". Sertakan sumbernya. Jawab ringkas dalam bahasa Indonesia."
	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
		log.Fatalf("send: %v", err)
	}
	<-done

	fmt.Println(strings.Repeat("─", 60))
	fmt.Println(answer)
}
