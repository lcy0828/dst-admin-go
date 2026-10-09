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
	"dont/internal/softwareupdate"
	"dont/internal/webui"
	"dont/pkg/configpath"
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
	if _, releaseBuild := softwareupdate.NormalizeVersion(buildinfo.Current().Version); releaseBuild && webui.Embedded() && softwareupdate.SupervisorAvailable() && !softwareupdate.ManagedChild() {
		config, err := configpath.Find()
		if err != nil {
			log.Fatalf("locate configuration: %v", err)
		}
		root, err := softwareupdate.DefaultRoot(config)
		if err != nil {
			log.Fatalf("locate software update storage: %v", err)
		}
		if err := softwareupdate.RunSupervisor(ctx, root, config, *address, buildinfo.Current().Version, os.Args[1:]); err != nil {
			log.Fatalf("software launcher: %v", err)
		}
		return
	}
	log.Printf("DST Admin API listening on http://%s", *address)
	if err := adminserver.Run(ctx, *address); err != nil {
		log.Fatalf("serve DST Admin API: %v", err)
	}
}
