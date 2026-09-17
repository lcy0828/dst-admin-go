package luajit

import (
	"context"
	"dont/shared"
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

//go:embed packages/*
var bundled embed.FS

func DefaultReleaseDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("DST_ADMIN_LUAJIT_RELEASE_DIR")); value != "" {
		return filepath.Abs(value)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dst-admin", "luajit-releases"), nil
}

// Reading the catalog never extracts packages or downloads anything.
type bundledPackage struct {
	File    string               `json:"file"`
	Release shared.LuaJITRelease `json:"release"`
}

func bundledPackages() ([]bundledPackage, error) {
	data, err := bundled.ReadFile("packages/manifest.json")
	if err != nil {
		return nil, err
	}
	var packages []bundledPackage
	err = json.Unmarshal(data, &packages)
	return packages, err
}
func (s *Store) Available() ([]shared.LuaJITRelease, error) {
	releases, err := s.List()
	if err != nil {
		return nil, err
	}
	packages, err := s.packageCatalog()
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	for index, r := range releases {
		seen[r.ID] = index
	}
	for _, p := range packages {
		if index, ok := seen[p.Release.ID]; ok {
			releases[index] = p.Release
		} else {
			seen[p.Release.ID] = len(releases)
			releases = append(releases, p.Release)
		}
	}
	sortReleases(releases)
	return releases, nil
}

// EnsureAvailable materializes a bundled package only for an explicit install.
func (s *Store) EnsureAvailable(ctx context.Context, id string) (shared.LuaJITRelease, error) {
	s.seedMu.Lock()
	defer s.seedMu.Unlock()
	if release, err := s.Get(id); err == nil {
		if _, err = os.Stat(filepath.Join(s.root, id+".zip")); err == nil {
			return release, nil
		}
	}
	packages, err := s.packageCatalog()
	if err != nil {
		return shared.LuaJITRelease{}, err
	}
	for _, p := range packages {
		if p.Release.ID != id {
			continue
		}
		if p.File == "" {
			release, err := s.ImportURL(ctx, p.Release.SourceURL, p.Release.SHA256)
			if err != nil {
				return release, err
			}
			if release.Version != p.Release.Version || release.Size != p.Release.Size {
				return shared.LuaJITRelease{}, ErrInvalidPackage
			}
			return release, nil
		}
		f, err := bundled.Open("packages/" + p.File)
		if err != nil {
			return shared.LuaJITRelease{}, err
		}
		defer f.Close()
		release, err := s.Save(ctx, f, id)
		if err != nil {
			return release, err
		}
		if release != p.Release {
			return shared.LuaJITRelease{}, ErrInvalidPackage
		}
		return release, nil
	}
	return shared.LuaJITRelease{}, os.ErrNotExist
}

// SeedBundled is for explicit packaging/installation tools, never catalog reads.
func (s *Store) SeedBundled(ctx context.Context) error {
	packages, err := bundledPackages()
	if err != nil {
		return err
	}
	for _, p := range packages {
		if _, err := s.EnsureAvailable(ctx, p.Release.ID); err != nil {
			return err
		}
	}
	return nil
}
