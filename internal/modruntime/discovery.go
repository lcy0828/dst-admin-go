package modruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Kind string

const (
	KindLua        Kind = "lua"
	KindPythonLupa Kind = "python-lupa"
	probeTimeout        = 5 * time.Second
)

type Runtime struct {
	Kind    Kind
	Path    string
	Source  string
	Version string
}

type Discovery struct {
	Runtimes         []Runtime
	ConfiguredLua    string
	ConfiguredPython string
	PythonAvailable  bool
	PythonLupa       bool
	Failures         []string
}

func (d Discovery) Primary() (Runtime, bool) {
	if len(d.Runtimes) == 0 {
		return Runtime{}, false
	}
	return d.Runtimes[0], true
}

func (d Discovery) Diagnostic() string {
	if primary, ok := d.Primary(); ok {
		return fmt.Sprintf("%s runtime available at %s", primary.Kind, primary.Path)
	}
	configured := make([]string, 0, 2)
	if d.ConfiguredLua != "" && !strings.EqualFold(d.ConfiguredLua, "auto") {
		configured = append(configured, fmt.Sprintf("configured Lua %q is not usable", d.ConfiguredLua))
	}
	if d.ConfiguredPython != "" && !strings.EqualFold(d.ConfiguredPython, "auto") {
		configured = append(configured, fmt.Sprintf("configured Python %q has no usable Lupa", d.ConfiguredPython))
	}
	if d.PythonAvailable && !d.PythonLupa {
		configured = append(configured, "Python is available but the lupa module is missing")
	}
	if len(configured) > 0 {
		return strings.Join(configured, "; ")
	}
	return "external Lua and Python/Lupa runtimes were not found"
}

type candidate struct {
	value  string
	source string
}

func Discover(luaBinary, pythonBinary string) Discovery {
	discovery := Discovery{
		ConfiguredLua:    strings.TrimSpace(luaBinary),
		ConfiguredPython: strings.TrimSpace(pythonBinary),
	}
	seen := make(map[string]bool)
	for _, item := range executableCandidates(discovery.ConfiguredLua, luaNames()) {
		path, ok := resolveExecutable(item.value)
		identity := executableIdentity(path)
		if !ok || seen[identity] {
			continue
		}
		seen[identity] = true
		version, err := probe(path, []string{"-e", "io.write(_VERSION)"})
		if err != nil {
			discovery.Failures = append(discovery.Failures, fmt.Sprintf("Lua %s: %v", path, err))
			continue
		}
		discovery.Runtimes = append(discovery.Runtimes, Runtime{Kind: KindLua, Path: path, Source: item.source, Version: version})
	}

	for _, item := range executableCandidates(discovery.ConfiguredPython, pythonNames()) {
		path, ok := resolveExecutable(item.value)
		identity := executableIdentity(path)
		if !ok || seen[identity] {
			continue
		}
		seen[identity] = true
		discovery.PythonAvailable = true
		version, err := probe(path, []string{"-c", "import lupa; print(lupa.__version__)"})
		if err != nil {
			discovery.Failures = append(discovery.Failures, fmt.Sprintf("Python %s: lupa unavailable: %v", path, err))
			continue
		}
		discovery.PythonLupa = true
		discovery.Runtimes = append(discovery.Runtimes, Runtime{Kind: KindPythonLupa, Path: path, Source: item.source, Version: version})
	}
	return discovery
}

func executableCandidates(configured string, names []string) []candidate {
	items := make([]candidate, 0, 1+len(names)*(1+len(standardExecutableDirectories())))
	if configured != "" && !strings.EqualFold(configured, "auto") {
		items = append(items, candidate{value: configured, source: "configured"})
	}
	for _, name := range names {
		items = append(items, candidate{value: name, source: "PATH"})
	}
	for _, directory := range standardExecutableDirectories() {
		for _, name := range names {
			items = append(items, candidate{value: filepath.Join(directory, name), source: "standard"})
		}
	}
	return items
}

func luaNames() []string {
	return []string{"lua", "lua5.5", "lua5.4", "lua5.3", "lua5.2", "lua5.1", "luajit"}
}

func pythonNames() []string {
	return []string{"python3", "python"}
}

func standardExecutableDirectories() []string {
	directories := []string{"/usr/local/bin", "/usr/bin", "/bin", "/opt/local/bin"}
	if runtime.GOOS == "darwin" {
		directories = append([]string{"/opt/homebrew/bin", "/opt/homebrew/opt/lua/bin", "/usr/local/opt/lua/bin"}, directories...)
	}
	return directories
}

func resolveExecutable(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	var path string
	if filepath.IsAbs(value) || strings.ContainsAny(value, `/\`) {
		path = value
	} else {
		resolved, err := exec.LookPath(value)
		if err != nil {
			return "", false
		}
		path = resolved
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return "", false
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
		return "", false
	}
	return absolute, true
}

func executableIdentity(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(resolved)
}

func probe(path string, arguments []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, arguments...)
	command.Env = probeEnvironment(path)
	output := &limitedOutput{limit: 4096}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("probe timed out")
		}
		message := strings.Join(strings.Fields(output.String()), " ")
		if message == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, message)
	}
	return strings.Join(strings.Fields(output.String()), " "), nil
}

func probeEnvironment(binary string) []string {
	path := strings.Join([]string{filepath.Dir(binary), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator))
	return []string{
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PATH=" + path,
		"LUA_INIT=", "LUA_INIT_5_1=", "LUA_INIT_5_2=", "LUA_INIT_5_3=", "LUA_INIT_5_4=", "LUA_INIT_5_5=",
	}
}

type limitedOutput struct {
	data  []byte
	limit int
}

func (w *limitedOutput) Write(value []byte) (int, error) {
	original := len(value)
	remaining := w.limit - len(w.data)
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		w.data = append(w.data, value...)
	}
	return original, nil
}

func (w *limitedOutput) String() string { return string(w.data) }
