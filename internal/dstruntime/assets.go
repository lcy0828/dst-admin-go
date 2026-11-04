package dstruntime

import "embed"

//go:embed assets/*.lua
var embeddedAssets embed.FS

var managedAssetNames = []string{"bootstrap.lua", "telemetry.lua", "worldstate.lua", "commands.lua", "events.lua", "diagnostics.lua", "barriers.lua", "activity.lua"}

func assetData(name string) ([]byte, error) {
	data, err := embeddedAssets.ReadFile("assets/" + name)
	if err != nil || name == "clock.lua" {
		return data, err
	}
	if name == "commands.lua" {
		for _, module := range []string{"mode_ownership.lua", "managed_modes.lua", "teleport.lua", "workbench.lua", "workbench_expansion.lua", "workbench_v2.lua", "player_settings.lua", "nearby_adapters.lua"} {
			part, e := embeddedAssets.ReadFile("assets/" + module)
			if e != nil {
				return nil, e
			}
			data = append(append(part, '\n'), data...)
		}
	}
	// Inline the shared clock: deployed file names and the restore layout stay
	// compatible with published collectors, including older Runtime backups.
	clock, err := embeddedAssets.ReadFile("assets/clock.lua")
	if err != nil {
		return nil, err
	}
	return append(append(clock, '\n'), data...), nil
}
