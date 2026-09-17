package shards

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const maximumPortableUnixSocketPathBytes = 100

// NativeConsoleSocketPath derives one stable tmux ownership boundary from the
// physical save root. Runtime installation labels and Agent state paths may
// change without moving a running shard to a different tmux server.
func NativeConsoleSocketPath(saveRoot string) (string, error) {
	saveRoot = filepath.Clean(strings.TrimSpace(saveRoot))
	if saveRoot == "" || saveRoot == "." || !filepath.IsAbs(saveRoot) || strings.ContainsAny(saveRoot, "\x00\r\n") {
		return "", errors.New("native Runtime save root must be an absolute safe path")
	}
	identity := saveRoot
	if resolved, err := filepath.EvalSymlinks(saveRoot); err == nil {
		identity = resolved
	}
	var path string
	if runtime.GOOS == "darwin" {
		digest := sha256.Sum256([]byte(identity))
		path = filepath.Join("/tmp", fmt.Sprintf("dst-admin-runtime-%d", os.Getuid()), "native-"+hex.EncodeToString(digest[:8])+".sock")
	} else {
		path = filepath.Join(saveRoot, ".dst-admin", "runtime", "tmux.sock")
	}
	if len([]byte(path)) > maximumPortableUnixSocketPathBytes {
		return "", fmt.Errorf("native Runtime tmux socket path is too long: %s", path)
	}
	return path, nil
}

func prepareNativeConsoleSocket(path string, managedDirectory bool) error {
	directory := filepath.Dir(path)
	info, err := os.Stat(directory)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("tmux socket parent is not a directory: %s", directory)
		}
		if managedDirectory && info.Mode().Perm() != 0o700 {
			if err := os.Chmod(directory, 0o700); err != nil {
				return fmt.Errorf("protect tmux socket directory: %w", err)
			}
		}
	case os.IsNotExist(err):
		mode := os.FileMode(0o755)
		if managedDirectory {
			mode = 0o700
		}
		if err := os.MkdirAll(directory, mode); err != nil {
			return fmt.Errorf("create tmux socket directory: %w", err)
		}
		if managedDirectory {
			if err := os.Chmod(directory, 0o700); err != nil {
				return fmt.Errorf("protect tmux socket directory: %w", err)
			}
		}
	case err != nil:
		return fmt.Errorf("inspect tmux socket directory: %w", err)
	}
	return nil
}

func nativeRuntimeStateDirectory(saveRoot string) string {
	return filepath.Join(saveRoot, ".dst-admin", "runtime")
}

// legacyDefaultConsoleSocketPaths finds tmux sockets that survived a systemd
// Agent restart in an older PrivateTmp mount namespace. The DST process and
// its tmux parent share a UID, so the socket remains reachable through the
// tmux process root without granting the Agent additional privileges.
func legacyDefaultConsoleSocketPaths(processIDs []int32) []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	result := make([]string, 0, len(processIDs))
	seen := make(map[string]struct{}, len(processIDs))
	for _, processID := range processIDs {
		parentID, _, ok := linuxProcessIdentity(processID)
		if !ok || parentID < 1 {
			continue
		}
		parentName, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(parentID), "comm"))
		if err != nil || strings.TrimSpace(string(parentName)) != "tmux" {
			continue
		}
		_, parentUID, ok := linuxProcessIdentity(int32(parentID))
		if !ok || parentUID != os.Getuid() {
			continue
		}
		path := filepath.Join("/proc", strconv.Itoa(parentID), "root", "tmp", fmt.Sprintf("tmux-%d", parentUID), "default")
		info, err := os.Stat(path)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		defaultPath := filepath.Join(os.TempDir(), fmt.Sprintf("tmux-%d", parentUID), "default")
		if defaultInfo, statErr := os.Stat(defaultPath); statErr == nil && os.SameFile(defaultInfo, info) {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func linuxProcessIdentity(processID int32) (parentID, uid int, ok bool) {
	if processID < 1 {
		return 0, 0, false
	}
	content, err := os.ReadFile(filepath.Join("/proc", strconv.FormatInt(int64(processID), 10), "status"))
	if err != nil {
		return 0, 0, false
	}
	return parseLinuxProcessIdentity(string(content))
}

func parseLinuxProcessIdentity(content string) (parentID, uid int, ok bool) {
	parentFound, uidFound := false, false
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "PPid:":
			value, err := strconv.Atoi(fields[1])
			if err == nil && value >= 0 {
				parentID, parentFound = value, true
			}
		case "Uid:":
			value, err := strconv.Atoi(fields[1])
			if err == nil && value >= 0 {
				uid, uidFound = value, true
			}
		}
	}
	return parentID, uid, parentFound && uidFound
}
