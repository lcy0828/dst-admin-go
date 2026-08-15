package configpath

import (
	"fmt"
	"os"
	"path/filepath"
)

const Environment = "DST_ADMIN_CONFIG"

// Find resolves the runtime INI without loading it, so shared packages can use
// the same path without triggering control-plane configuration side effects.
func Find() (string, error) {
	if configured := os.Getenv(Environment); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(absolute); err != nil {
			return "", err
		}
		return absolute, nil
	}

	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(current, "conf", "app.conf")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", fmt.Errorf("conf/app.conf was not found from the working directory or its parents; set %s", Environment)
}

// Current preserves legacy modules' defaults when no configuration exists.
// The control-plane startup remains strict through setting.findConfigPath.
func Current() string {
	path, err := Find()
	if err != nil {
		return filepath.Clean("conf/app.conf")
	}
	return path
}
