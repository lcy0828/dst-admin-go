package dstruntime

import "embed"

//go:embed assets/*.lua
var embeddedAssets embed.FS

var managedAssetNames = []string{"bootstrap.lua", "telemetry.lua", "commands.lua", "events.lua", "diagnostics.lua"}

func assetData(name string) ([]byte, error) {
	return embeddedAssets.ReadFile("assets/" + name)
}
