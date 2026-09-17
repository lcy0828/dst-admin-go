//go:build !webui

package webui

import "io/fs"

func embeddedAssets() fs.FS { return nil }

func Embedded() bool { return false }
