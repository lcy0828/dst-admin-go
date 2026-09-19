package architecture

import (
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestProductionCodeDoesNotBypassRuntimeDriverForTmux(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve architecture test path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	allowed := map[string]bool{
		"internal/shards/tmux_control.go": true,
		"agent/container_runtime.go":      true,
	}
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			// Match go tooling's treatment of hidden/private source copies and
			// avoid walking frontend dependencies or generated build outputs.
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "vendor" || name == "node_modules" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		directory, name := filepath.Dir(path), filepath.Base(path)
		matches, matchErr := build.Default.MatchFile(directory, name)
		if matchErr != nil || !matches {
			return matchErr
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if allowed[relative] || strings.HasPrefix(relative, "tmux/") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imported := range parsed.Imports {
			value, unquoteErr := strconv.Unquote(imported.Path.Value)
			if unquoteErr != nil {
				return unquoteErr
			}
			if value == "dont/tmux" || value == "github.com/GianlucaP106/gotmux/gotmux" {
				violations = append(violations, relative+" imports "+value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("tmux is a low-level Runtime adapter; high-level production code must use Runtime Driver:\n%s", strings.Join(violations, "\n"))
	}
}

func TestProductionRouterDoesNotRegisterLegacyRuntimePackages(t *testing.T) {
	_, currentFile, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "routers", "router.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	blocked := map[string]bool{
		"dont/routers/tmux": true, "dont/routers/parser": true,
		"dont/routers/dstserver": true, "dont/routers/mod": true,
	}
	for _, imported := range parsed.Imports {
		value, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if blocked[value] {
			t.Fatalf("production router imports legacy Runtime package %s", value)
		}
	}
}
