package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"dont/internal/dstserver"
	"dont/internal/moddistribution"
)

func main() {
	server := flag.String("server-path", "", "DST server directory")
	content := flag.String("workshop-content-path", "", "SteamCMD Workshop content/322330 directory")
	apply := flag.Bool("apply", false, "create missing local Mod links; never overwrite existing files")
	flag.Parse()
	if err := run(*server, *content, *apply); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(server, content string, apply bool) error {
	if !filepath.IsAbs(server) || !filepath.IsAbs(content) {
		return fmt.Errorf("both configured paths must be absolute")
	}
	if layout, ok := dstserver.Resolve(server, "64"); ok {
		server = layout.ContentRoot
	}
	server, err := filepath.EvalSymlinks(server)
	if err != nil {
		return err
	}
	content, err = filepath.EvalSymlinks(content)
	if err != nil {
		return err
	}
	ctx := context.Background()
	observation, err := moddistribution.InventoryInstallationFiles(ctx, moddistribution.TrustedInstallation{
		ID: "local-setup", ServerPath: server, SavePath: server, WorkshopContentPath: content,
	})
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(observation.Mods))
	for id, state := range observation.Mods {
		if state.Status != moddistribution.FileReady {
			return fmt.Errorf("Workshop %s cannot be linked: %s %s", id, state.Status, state.Reason)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Printf("Local Workshop Mods (%d): %v\n", len(ids), ids)
	if !apply {
		fmt.Println("Dry run only. Use -apply to create missing links.")
		return nil
	}
	if err := moddistribution.LinkWorkshopMods(ctx, server, content, ids); err != nil {
		return err
	}
	fmt.Println("Local Mod entries ready. No downloads, configuration changes or world restarts.")
	return nil
}
