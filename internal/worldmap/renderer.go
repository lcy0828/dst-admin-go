package worldmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"dont/internal/maprenderer"
)

const (
	rendererProbeTimeout = 3 * time.Second
	rendererProbeTTL     = 30 * time.Second
	maxProbeOutput       = 64 * 1024
)

var ErrRendererUnavailable = errors.New("map renderer is unavailable")

type Renderer interface {
	Available() (bool, string)
	Render(context.Context, string, string, []Layer, io.Writer) error
}

type RendererInfo struct {
	Available       bool     `json:"available"`
	Path            string   `json:"path,omitempty"`
	ProtocolVersion string   `json:"protocolVersion,omitempty"`
	Version         string   `json:"version,omitempty"`
	Artifacts       []string `json:"artifacts"`
	Error           string   `json:"error,omitempty"`
}

type rendererProbe struct {
	ProtocolVersion string `json:"protocolVersion"`
	RendererVersion string `json:"rendererVersion"`
	Capabilities    struct {
		Artifacts []string `json:"artifacts"`
	} `json:"capabilities"`
}

type ExecRenderer struct {
	configuredPath string
	assetsPath     string
	mu             sync.Mutex
	cachedAt       time.Time
	cachedPath     string
	cachedInfo     RendererInfo
}

func NewExecRenderer(configuredPath string, assetsPath ...string) *ExecRenderer {
	value := ""
	if len(assetsPath) > 0 {
		value = strings.TrimSpace(assetsPath[0])
	}
	return &ExecRenderer{configuredPath: strings.TrimSpace(configuredPath), assetsPath: value}
}

func (r *ExecRenderer) Available() (bool, string) {
	info := r.Info()
	return info.Available, info.Path
}

func (r *ExecRenderer) Info() RendererInfo {
	path := resolveRenderer(r.configuredPath)
	if path == "" {
		return RendererInfo{Artifacts: []string{}, Error: ErrRendererUnavailable.Error()}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if path == r.cachedPath && time.Since(r.cachedAt) < rendererProbeTTL {
		return r.cachedInfo
	}
	ctx, cancel := context.WithTimeout(context.Background(), rendererProbeTimeout)
	defer cancel()
	info := probeRenderer(ctx, path, r.assetsPath)
	r.cachedAt, r.cachedPath, r.cachedInfo = time.Now(), path, info
	return info
}

func probeRenderer(ctx context.Context, path, assetsPath string) RendererInfo {
	command := exec.CommandContext(ctx, path, "--probe", "--assets", assetsPath)
	command.Env = rendererEnvironment()
	var output bytes.Buffer
	command.Stdout = &limitedBuffer{buffer: &output, remaining: maxProbeOutput}
	command.Stderr = command.Stdout
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return RendererInfo{Path: path, Artifacts: []string{}, Error: "renderer capability probe timed out"}
		}
		return RendererInfo{Path: path, Artifacts: []string{}, Error: "renderer capability probe failed"}
	}
	var probe rendererProbe
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	if err := decoder.Decode(&probe); err != nil {
		return RendererInfo{Path: path, Artifacts: []string{}, Error: "renderer returned an invalid capability document"}
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RendererInfo{Path: path, Artifacts: []string{}, Error: "renderer returned trailing capability data"}
	}
	if probe.ProtocolVersion != maprenderer.ProtocolVersion {
		return RendererInfo{
			Path: path, ProtocolVersion: probe.ProtocolVersion, Version: probe.RendererVersion,
			Artifacts: append([]string(nil), probe.Capabilities.Artifacts...),
			Error:     fmt.Sprintf("renderer protocol %q is incompatible with required protocol %q", probe.ProtocolVersion, maprenderer.ProtocolVersion),
		}
	}
	if strings.TrimSpace(probe.RendererVersion) == "" {
		return RendererInfo{Path: path, ProtocolVersion: probe.ProtocolVersion, Artifacts: append([]string(nil), probe.Capabilities.Artifacts...), Error: "renderer version is missing"}
	}
	for _, required := range []string{maprenderer.TerrainFileName, maprenderer.ManifestFileName, maprenderer.FeaturesFileName} {
		if !containsString(probe.Capabilities.Artifacts, required) {
			return RendererInfo{
				Path: path, ProtocolVersion: probe.ProtocolVersion, Version: probe.RendererVersion,
				Artifacts: append([]string(nil), probe.Capabilities.Artifacts...), Error: "renderer does not provide required v1 artifacts",
			}
		}
	}
	return RendererInfo{
		Available: true, Path: path, ProtocolVersion: probe.ProtocolVersion,
		Version: probe.RendererVersion, Artifacts: append([]string(nil), probe.Capabilities.Artifacts...),
	}
}

func (r *ExecRenderer) Render(ctx context.Context, input, output string, layers []Layer, log io.Writer) error {
	info := r.Info()
	if !info.Available {
		return fmt.Errorf("%w: %s", ErrRendererUnavailable, info.Error)
	}
	values := make([]string, len(layers))
	for index, layer := range layers {
		values[index] = string(layer)
	}
	arguments := []string{
		"--input", input,
		"--output", output,
		"--assets", r.assetsPath,
		"--layers", strings.Join(values, ","),
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			arguments = append(arguments, "--timeout", remaining.String())
		}
	}
	command := exec.CommandContext(ctx, info.Path, arguments...)
	command.Env = rendererEnvironment()
	command.Dir = output
	command.Stdout = log
	command.Stderr = log
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("map renderer timed out")
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return context.Canceled
		}
		return fmt.Errorf("map renderer process failed: %w", err)
	}
	return nil
}

func resolveRenderer(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if path := executableAt(configured); path != "" {
			return path
		}
		if info, err := os.Stat(configured); err == nil && info.IsDir() {
			for _, name := range rendererNames() {
				if path := executableAt(filepath.Join(configured, name)); path != "" {
					return path
				}
			}
		}
		if !strings.ContainsAny(configured, `/\\`) {
			if value, err := exec.LookPath(configured); err == nil {
				return value
			}
		}
		return ""
	}
	if current, err := os.Executable(); err == nil {
		for _, name := range rendererNames() {
			if path := executableAt(filepath.Join(filepath.Dir(current), name)); path != "" {
				return path
			}
		}
	}
	for _, name := range rendererNames() {
		if value, err := exec.LookPath(name); err == nil {
			return value
		}
	}
	return ""
}

func executableAt(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
		return ""
	}
	return filepath.Clean(path)
}

func rendererNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"dst-map-renderer.exe", "dst-map-renderer"}
	}
	return []string{"dst-map-renderer", "dst-map-renderer.exe"}
}

func rendererEnvironment() []string {
	allowed := map[string]bool{
		"PATH": true, "LANG": true, "LC_ALL": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"SYSTEMROOT": true, "WINDIR": true, "DST_MAP_TEST_ARGUMENTS": true,
	}
	result := make([]string, 0, len(allowed))
	for _, value := range os.Environ() {
		name, _, ok := strings.Cut(value, "=")
		if ok && allowed[strings.ToUpper(name)] {
			result = append(result, value)
		}
	}
	return result
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type limitedBuffer struct {
	buffer    *bytes.Buffer
	remaining int
}

func (w *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > w.remaining {
		value = value[:w.remaining]
	}
	w.remaining -= len(value)
	_, _ = w.buffer.Write(value)
	if original > len(value) {
		return len(value), errors.New("renderer capability output exceeds limit")
	}
	return original, nil
}

func layerFileName(layer Layer) string {
	return map[Layer]string{LayerTerrain: maprenderer.TerrainFileName}[layer]
}
