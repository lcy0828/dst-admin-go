package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"

	"dont/internal/adminserver"
	"dont/pkg/setting"
)

func main() {
	defaultAddress := fmt.Sprintf("127.0.0.1:%d", setting.HTTPPort)
	address := flag.String("addr", defaultAddress, "HTTP listen address")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("DST Admin API listening on http://%s", *address)
	if err := adminserver.Run(ctx, *address); err != nil {
		log.Fatalf("serve DST Admin API: %v", err)
	}
}
