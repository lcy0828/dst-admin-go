package maprenderer

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxAssetFileSize = int64(64 * 1024 * 1024)

type AssetSource struct {
	DataRoot string
}

func DiscoverAssets(configured string) (*AssetSource, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return nil, errors.New("DST asset path is required")
	}
	candidates := []string{
		configured,
		filepath.Join(configured, "data"),
		filepath.Join(configured, "dontstarve_steam.app", "Contents", "data"),
		filepath.Join(configured, "Contents", "data"),
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates,
			filepath.Join(configured, "Don't Starve Together", "dontstarve_steam.app", "Contents", "data"),
		)
	}
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if validAssetRoot(candidate) {
			return &AssetSource{DataRoot: candidate}, nil
		}
	}
	return nil, fmt.Errorf("DST assets were not found below %q", configured)
}

func validAssetRoot(root string) bool {
	required := []string{
		filepath.Join("databundles", "scripts.zip"),
		filepath.Join("databundles", "images.zip"),
		filepath.Join("levels", "tiles", "map_edge.tex"),
		filepath.Join("levels", "tiles", "map_edge.xml"),
		filepath.Join("minimap", "minimap_atlas.tex"),
		filepath.Join("minimap", "minimap_data.xml"),
	}
	for _, relative := range required {
		info, err := os.Stat(filepath.Join(root, relative))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func (a *AssetSource) Read(relative string) ([]byte, error) {
	if a == nil || !safeAssetPath(relative) {
		return nil, errors.New("invalid DST asset path")
	}
	path := filepath.Join(a.DataRoot, filepath.FromSlash(relative))
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open DST asset %s: %w", relative, err)
	}
	defer file.Close()
	return readLimited(file, maxAssetFileSize, "DST asset "+relative)
}

func (a *AssetSource) ReadScript(relative string) ([]byte, error) {
	return a.readBundle("scripts.zip", "scripts/"+strings.TrimPrefix(relative, "scripts/"))
}

func (a *AssetSource) ReadImage(relative string) ([]byte, error) {
	return a.readBundle("images.zip", "images/"+strings.TrimPrefix(relative, "images/"))
}

func (a *AssetSource) ReadLevelTexture(name string) ([]byte, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".tex") + ".tex"
	directory := filepath.Join(a.DataRoot, "levels", "textures")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read DST level textures: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(entry.Name(), name) {
			return a.Read(filepath.ToSlash(filepath.Join("levels", "textures", entry.Name())))
		}
	}
	return nil, fmt.Errorf("DST level texture %s was not found", name)
}

func (a *AssetSource) readBundle(bundle, name string) ([]byte, error) {
	if a == nil || !safeAssetPath(name) {
		return nil, errors.New("invalid DST bundle asset path")
	}
	reader, err := zip.OpenReader(filepath.Join(a.DataRoot, "databundles", bundle))
	if err != nil {
		return nil, fmt.Errorf("open DST bundle %s: %w", bundle, err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if entry.Name != name {
			continue
		}
		if entry.FileInfo().IsDir() || entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > uint64(maxAssetFileSize) {
			return nil, fmt.Errorf("DST bundle asset %s has an invalid size", name)
		}
		file, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("open DST bundle asset %s: %w", name, err)
		}
		data, readErr := readLimited(file, maxAssetFileSize, "DST bundle asset "+name)
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close DST bundle asset %s: %w", name, closeErr)
		}
		return data, nil
	}
	return nil, fmt.Errorf("DST bundle asset %s was not found", name)
}

func safeAssetPath(value string) bool {
	value = filepath.ToSlash(strings.TrimSpace(value))
	return value != "" && value != "." && !strings.HasPrefix(value, "/") && !strings.Contains(value, "../") && !strings.ContainsRune(value, '\x00')
}

func readZipEntry(entry *zip.File) ([]byte, error) {
	if entry == nil || entry.UncompressedSize64 > uint64(maxAssetFileSize) {
		return nil, errors.New("zip entry exceeds the asset size limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, maxAssetFileSize+1))
}
