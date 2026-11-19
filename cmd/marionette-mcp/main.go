package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"marionette/client"
	"marionette/internal/mcpbridge"
)

func main() {
	log.SetFlags(0) // Logs go to stderr; stdout belongs exclusively to MCP.
	base := os.Getenv("MARIONETTE_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	url := flag.String("url", base, "Marionette API origin (or MARIONETTE_URL)")
	flag.Parse()
	api, err := client.New(*url, os.Getenv("MARIONETTE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server, bridge := mcpbridge.New(api)
	err = server.Run(ctx, &mcp.StdioTransport{})
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cleanupErr := bridge.Close(cleanup); cleanupErr != nil {
		log.Print(cleanupErr)
	}
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
