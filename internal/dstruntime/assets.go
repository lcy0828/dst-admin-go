package dstruntime

import "embed"

//go:embed assets/*.lua
var embeddedAssets embed.FS

var managedAssetNames = []string{"bootstrap.lua", "telemetry.lua", "worldstate.lua", "commands.lua", "events.lua", "diagnostics.lua", "barriers.lua"}

func assetData(name string) ([]byte, error) {
	return embeddedAssets.ReadFile("assets/" + name)
}
