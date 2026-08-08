package worldmap

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrRendererUnavailable = errors.New("map renderer is unavailable")

type Renderer interface {
	Available() (bool, string)
	Render(context.Context, string, string, []Layer, io.Writer) error
}

type ExecRenderer struct{ configuredPath string }

func NewExecRenderer(configuredPath string) *ExecRenderer {
	return &ExecRenderer{configuredPath: strings.TrimSpace(configuredPath)}
}

func (r *ExecRenderer) Available() (bool, string) {
	path := resolveRenderer(r.configuredPath)
	return path != "", path
}

func (r *ExecRenderer) Render(ctx context.Context, input, output string, layers []Layer, log io.Writer) error {
	_, executable := r.Available()
	if executable == "" {
		return ErrRendererUnavailable
	}
	values := make([]string, len(layers))
	for index, layer := range layers {
		values[index] = string(layer)
	}
	command := exec.CommandContext(ctx, executable,
		"--input", input,
		"--output", output,
		"--layers", strings.Join(values, ","),
	)
	command.Stdout = log
	command.Stderr = log
	return command.Run()
}

func resolveRenderer(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		info, err := os.Stat(configured)
		if err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0) {
			return filepath.Clean(configured)
		}
		if err == nil && info.IsDir() {
			for _, name := range []string{"dst-map-renderer", "dst-map-renderer.exe"} {
				candidate := filepath.Join(configured, name)
				if candidateInfo, candidateErr := os.Stat(candidate); candidateErr == nil && !candidateInfo.IsDir() && (runtime.GOOS == "windows" || candidateInfo.Mode().Perm()&0111 != 0) {
					return candidate
				}
			}
		}
	}
	for _, name := range []string{"dst-map-renderer", "dst-map-renderer.exe"} {
		if value, err := exec.LookPath(name); err == nil {
			return value
		}
	}
	return ""
}

func layerFileName(layer Layer) string {
	return map[Layer]string{
		LayerTerrain: "terrain.png", LayerWalrusCamps: "walrus-camps.png", LayerSpawnPoints: "spawn-points.png",
		LayerPlayers: "players.png", LayerWorldState: "world-state.png",
	}[layer]
}
