package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"dont/internal/adminserver"
	"dont/internal/buildinfo"
	"dont/internal/webui"
)

func main() {
	address := flag.String("addr", "127.0.0.1:18000", "HTTP listen address")
	version := flag.Bool("version", false, "print build metadata and exit")
	flag.Parse()
	if *version {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			buildinfo.Info
			EmbeddedWebUI bool `json:"embeddedWebUI"`
		}{buildinfo.Current(), webui.Embedded()})
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("DST Admin API listening on http://%s", *address)
	if err := adminserver.Run(ctx, *address); err != nil {
		log.Fatalf("serve DST Admin API: %v", err)
	}
}
