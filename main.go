package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"

	"dont/internal/adminserver"
	"dont/internal/buildinfo"
	"dont/internal/servicelogs"
	"dont/pkg/configpath"
	"dont/pkg/setting"
)

func main() {
	closeLogs := servicelogs.Start(servicelogs.Config{Root: servicelogs.Root(configpath.Current()), Component: "controller", Version: buildinfo.Current().Version}, false)
	defer closeLogs()
	if err := setting.Validate(); err != nil {
		servicelogs.Fatal(err)
	}
	defaultAddress := fmt.Sprintf("127.0.0.1:%d", setting.HTTPPort)
	address := flag.String("addr", defaultAddress, "HTTP listen address")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("DST Admin API listening on http://%s", *address)
	if err := adminserver.Run(ctx, *address); err != nil {
		servicelogs.Fatalf("serve DST Admin API: %v", err)
	}
}
