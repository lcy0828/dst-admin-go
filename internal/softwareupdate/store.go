package softwareupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var releaseIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var regexpFrontendCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

var bundleFiles = map[string]bool{"dst-admin": true, "dst-map-renderer": true, "mod-local-setup": true}

func filesFor(kind, platform string) map[string]bool {
	if kind != "agent" {
		return bundleFiles
	}
	suffix := ""
	if strings.HasPrefix(platform, "windows-") {
		suffix = ".exe"
	}
	return map[string]bool{"dst-admin-agent" + suffix: true, "dst-map-renderer" + suffix: true, "mod-local-setup" + suffix: true}
}

func binaryFor(kind, platform string) string {
	if kind != "agent" {
		return "dst-admin"
	}
	if strings.HasPrefix(platform, "windows-") {
		return "dst-admin-agent.exe"
	}
	return "dst-admin-agent"
}

func DefaultRoot(configPath string) (string, error) {
	if root := strings.TrimSpace(os.Getenv("DST_ADMIN_UPDATE_DIR")); root != "" {
		return filepath.Abs(root)
	}
	if configPath == "" {
		return "", fmt.Errorf("software update requires a configuration directory")
	}
	return filepath.Abs(filepath.Join(filepath.Dir(configPath), "software-updates"))
}

func ensureRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return ErrInvalid
	}
	for _, directory := range []string{root, filepath.Join(root, "releases")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe software update directory: %s", directory)
		}
		if err := os.Chmod(directory, 0700); err != nil {
			return err
		}
	}
	return nil
}

func regularRead(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = ErrInvalid
	}
	return data, err
}

func readState(root string) (diskState, error) {
	data, err := regularRead(filepath.Join(root, "state.json"), 64<<10)
	if os.IsNotExist(err) {
		return diskState{Protocol: Protocol}, nil
	}
	if err != nil {
		return diskState{}, err
	}
	var state diskState
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.Protocol != Protocol {
		return state, ErrInvalid
	}
	if state.BaseVersion != "" {
		if _, valid := NormalizeVersion(state.BaseVersion); !valid {
			return state, ErrInvalid
		}
	}
	for _, release := range []*installedRelease{state.Current, state.Previous, state.Pending} {
		if release != nil {
			if _, valid := NormalizeVersion(release.Version); !valid || release.ID != "" && !releaseIDPattern.MatchString(release.ID) {
				return state, ErrInvalid
			}
		}
	}
	if state.Operation != nil {
		if !releaseIDPattern.MatchString(state.Operation.ID) || state.Operation.ReleaseID != "" && !releaseIDPattern.MatchString(state.Operation.ReleaseID) {
			return state, ErrInvalid
		}
	}
	return state, nil
}

// BundledHelper redirects only the packaged default. An operator's custom
// executable path remains authoritative.
func BundledHelper(configured, name string) string {
	if !ManagedChild() || !bundleFiles[name] || name == "dst-admin" {
		return configured
	}
	binary, err := os.Executable()
	if err != nil {
		return configured
	}
	candidate := filepath.Join(filepath.Dir(binary), name)
	if configured != "" && configured != name && configured != filepath.Join("/usr/local/bin", name) && filepath.Clean(configured) != candidate {
		return configured
	}
	if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
		return candidate
	}
	return configured
}

func writeJSON(path string, value any) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return ErrInvalid
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return syncDirectory(directory)
}

func writeState(root string, state diskState) error {
	return writeJSON(filepath.Join(root, "state.json"), state)
}

type readyMarker struct {
	ParentPID int    `json:"parentPid"`
	BootID    string `json:"bootId"`
}

func supervisorReady(root string) bool {
	if !ManagedChild() {
		return false
	}
	data, err := regularRead(filepath.Join(root, "ready.json"), 4096)
	if err != nil {
		return false
	}
	var marker readyMarker
	return json.Unmarshal(data, &marker) == nil && marker.ParentPID == os.Getppid() && marker.BootID != "" && marker.BootID == os.Getenv("DST_ADMIN_BOOT_ID")
}

func releaseDirectory(root, id string) (string, error) {
	if !releaseIDPattern.MatchString(id) {
		return "", ErrInvalid
	}
	directory := filepath.Join(root, "releases", id)
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrInvalid
	}
	return directory, nil
}

func validateBundle(directory, version, platform string) (Manifest, error) {
	return validateBundleFor(directory, version, platform, "")
}

func validateBundleFor(directory, version, platform, kind string) (Manifest, error) {
	files := filesFor(kind, platform)
	var manifest Manifest
	data, err := regularRead(filepath.Join(directory, "manifest.json"), 64<<10)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Protocol != Protocol || manifest.Version != version || manifest.Platform != platform || manifest.Kind != kind || len(manifest.Files) != len(files) || kind != "agent" && (!manifest.EmbeddedWebUI || !regexpFrontendCommit.MatchString(manifest.FrontendCommit)) {
		return manifest, fmt.Errorf("%w: manifest contract mismatch (protocol %d, version %s, platform %s; expected %d, %s, %s)", ErrInvalid, manifest.Protocol, manifest.Version, manifest.Platform, Protocol, version, platform)
	}
	var total int64
	for name := range files {
		entry, ok := manifest.Files[name]
		if !ok || entry.Size < 1 || entry.Size > MaxExtractedBytes || !digestPattern.MatchString(entry.SHA256) {
			return manifest, fmt.Errorf("%w: invalid metadata for %s", ErrInvalid, name)
		}
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil {
			return manifest, err
		}
		if !info.Mode().IsRegular() || info.Size() != entry.Size || !strings.HasPrefix(platform, "windows-") && info.Mode()&0111 == 0 {
			return manifest, fmt.Errorf("%w: %s must be a regular executable of %d bytes (actual %d bytes, mode %s)", ErrInvalid, name, entry.Size, info.Size(), info.Mode())
		}
		total += entry.Size
		if total > MaxExtractedBytes {
			return manifest, ErrInvalid
		}
		f, err := os.Open(filepath.Join(directory, name))
		if err != nil {
			return manifest, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, io.LimitReader(f, entry.Size+1))
		closeErr := f.Close()
		if copyErr != nil {
			return manifest, copyErr
		}
		if closeErr != nil {
			return manifest, closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return manifest, fmt.Errorf("%w: %s checksum mismatch", ErrInvalid, name)
		}
	}
	return manifest, nil
}
