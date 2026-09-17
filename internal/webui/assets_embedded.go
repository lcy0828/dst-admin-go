//go:build webui

package webui

import (
	"embed"
	"io/fs"
)

// The packaging scripts create dist from the selected frontend revision.
// A release build fails at compile time if the frontend was not prepared.
//
//go:embed all:dist
var releaseAssets embed.FS

func embeddedAssets() fs.FS {
	assets, err := fs.Sub(releaseAssets, "dist")
	if err != nil {
		panic(err)
	}
	return assets
}

func Embedded() bool { return true }
