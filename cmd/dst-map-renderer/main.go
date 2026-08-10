package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"dont/internal/maprenderer"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dst-map-renderer:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("dst-map-renderer", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	probe := flags.Bool("probe", false, "print renderer capabilities as JSON")
	input := flags.String("input", "", "read-only Session snapshot")
	output := flags.String("output", "", "empty output directory")
	assets := flags.String("assets", "", "DST installation or data directory")
	layers := flags.String("layers", "terrain", "legacy layer selection")
	timeout := flags.Duration("timeout", 90*time.Second, "render timeout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not supported")
	}
	renderer := maprenderer.New(version)
	renderer.AssetsPath = strings.TrimSpace(*assets)
	if *probe {
		if _, err := maprenderer.DiscoverAssets(renderer.AssetsPath); err != nil {
			return fmt.Errorf("official DST assets are unavailable: %w", err)
		}
		return json.NewEncoder(os.Stdout).Encode(renderer.Probe())
	}
	if strings.TrimSpace(*input) == "" || strings.TrimSpace(*output) == "" {
		return errors.New("--input and --output are required")
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		return errors.New("--timeout must be between 1ns and 10m")
	}
	_ = layers // Accepted for protocol compatibility; Renderer v1 always emits complete structured artifacts.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	manifest, err := renderer.Render(ctx, *input, *output)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "rendered %dx%d terrain with %d features\n", manifest.Map.ImageWidth, manifest.Map.ImageHeight, manifest.Statistics.FeatureCount)
	return nil
}
