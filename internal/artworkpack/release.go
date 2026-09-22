// Package artworkpack manages optional, controller-local workbench artwork.
// It never reads or writes game installations or saves.
package artworkpack

import "errors"

const MaxArchiveBytes int64 = 20 << 20

var (
	ErrBusy         = errors.New("artwork package operation in progress")
	ErrSource       = errors.New("unknown artwork package download source")
	ErrInvalid      = errors.New("invalid artwork package")
	ErrNotInstalled = errors.New("artwork package not installed")
)

type Release struct {
	Version        string            `json:"version"`
	GameVersion    string            `json:"gameVersion"`
	SHA256         string            `json:"sha256"`
	ArchiveBytes   int64             `json:"archiveBytes"`
	InstalledBytes int64             `json:"installedBytes"`
	Entities       int               `json:"entities"`
	Illustrated    int               `json:"illustrated"`
	Images         int               `json:"images"`
	Sources        map[string]string `json:"sources"`
}

// Releases are pinned in the application so proxies and offline imports have
// the same integrity check. Publishing a new pack does not replace this asset.
func OfficialRelease() Release {
	const url = "https://github.com/lcy0828/dst-admin-go/releases/download/artwork-747465.1/dst-workbench-artwork-747465.1.tar.gz"
	return Release{
		Version: "747465.1", GameVersion: "747465", SHA256: "8e4fe16d7adcb54099e2f8e3459a5b0e3ba4fe7dc5a381f2c3d7b771907853e7",
		ArchiveBytes: 11345032, InstalledBytes: 11865815, Entities: 6121, Illustrated: 2441, Images: 2414,
		Sources: map[string]string{"domestic": "https://ghfast.top/" + url, "github": url},
	}
}
