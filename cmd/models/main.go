package main

import (
	"context"
	"fmt"
	"log"

	copilot "github.com/github/copilot-sdk/go"
)

func main() {
	ctx := context.Background()
	client := copilot.NewClient(&copilot.ClientOptions{LogLevel: "error"})
	if err := client.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer client.Stop()

	models, err := client.ListModels(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range models {
		fmt.Printf("%-28s %s\n", m.ID, m.Name)
	}
}
