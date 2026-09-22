package artworkpack

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var imagePattern = regexp.MustCompile(`^images/[a-f0-9]{24}\.webp$`)
var prefabPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,79}$`)

type catalog struct {
	GameVersion string `json:"gameVersion"`
	Entities    int    `json:"entities"`
	Illustrated int    `json:"illustrated"`
	Items       []struct {
		ID       string `json:"id"`
		NameZhCN string `json:"nameZhCN"`
		NameEn   string `json:"nameEn"`
		Image    string `json:"image"`
		Type     string `json:"type"`
	} `json:"items"`
}

func extract(ctx context.Context, input io.Reader, dest string) (int64, error) {
	gz, err := gzip.NewReader(input)
	if err != nil {
		return 0, ErrInvalid
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, 40<<20))
	seen := map[string]bool{}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, ErrInvalid
		}
		if header.Typeflag == tar.TypeDir && (strings.TrimSuffix(header.Name, "/") == "workbench" || strings.TrimSuffix(header.Name, "/") == "workbench/images") {
			continue
		}
		if header.Typeflag != tar.TypeReg || !strings.HasPrefix(header.Name, "workbench/") {
			return 0, ErrInvalid
		}
		name := strings.TrimPrefix(header.Name, "workbench/")
		limit := int64(256 << 10)
		switch name {
		case "catalog.json":
			limit = 4 << 20
		case "README.txt":
			limit = 64 << 10
		default:
			if !imagePattern.MatchString(name) {
				return 0, ErrInvalid
			}
		}
		total += header.Size
		if seen[name] || len(seen) >= 10000 || header.Size < 0 || header.Size > limit || total > 32<<20 {
			return 0, ErrInvalid
		}
		seen[name] = true
		filename := filepath.Join(dest, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			return 0, err
		}
		file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return 0, err
		}
		_, copyErr := io.Copy(file, reader)
		closeErr := file.Close()
		if err = errors.Join(copyErr, closeErr); err != nil {
			return 0, err
		}
	}
	// Force the gzip checksum to be read, rejecting truncated/corrupt trailers.
	if _, err = io.Copy(io.Discard, io.LimitReader(gz, 1)); err != nil {
		return 0, ErrInvalid
	}
	return total, nil
}

func (a *activePack) load(root string) error {
	info, err := os.Lstat(filepath.Join(root, "catalog.json"))
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return ErrInvalid
	}
	data, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &a.catalog) != nil || a.catalog.GameVersion != a.Installed.GameVersion || len(a.catalog.Items) != a.Installed.Entities || a.catalog.Entities != a.Installed.Entities || a.catalog.Illustrated != a.Installed.Illustrated {
		return ErrInvalid
	}
	a.artwork = map[string]string{}
	ids := map[string]bool{}
	images := map[string]bool{}
	for _, item := range a.catalog.Items {
		if !prefabPattern.MatchString(item.ID) || ids[item.ID] {
			return ErrInvalid
		}
		ids[item.ID] = true
		if item.Image == "" {
			continue
		}
		if !imagePattern.MatchString(item.Image) {
			return ErrInvalid
		}
		a.artwork[item.ID] = item.Image
		if images[item.Image] {
			continue
		}
		images[item.Image] = true
		name := filepath.Join(root, filepath.FromSlash(item.Image))
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 12 || info.Size() > 256<<10 {
			return ErrInvalid
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		header := make([]byte, 12)
		_, err = io.ReadFull(file, header)
		file.Close()
		if err != nil || string(header[:4]) != "RIFF" || string(header[8:]) != "WEBP" {
			return ErrInvalid
		}
	}
	if len(a.artwork) != a.Installed.Illustrated || len(images) != a.Installed.Images {
		return ErrInvalid
	}
	return nil
}
