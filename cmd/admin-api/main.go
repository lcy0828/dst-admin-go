package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"dont/internal/adminserver"
)

func main() {
	address := flag.String("addr", "127.0.0.1:18000", "HTTP listen address")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("DST Admin API listening on http://%s", *address)
	if err := adminserver.Run(ctx, *address); err != nil {
		log.Fatalf("serve DST Admin API: %v", err)
	}
}
