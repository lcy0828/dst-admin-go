package template

import (
	_ "embed"
	"fmt"
)

//go:embed forest/leveldataoverride.lua
var forestLevelDataOverride []byte

//go:embed cave/leveldataoverride.lua
var caveLevelDataOverride []byte

// LevelDataOverride returns a complete DST level definition for a new world.
func LevelDataOverride(worldType string) ([]byte, error) {
	var source []byte
	switch worldType {
	case "forest":
		source = forestLevelDataOverride
	case "cave":
		source = caveLevelDataOverride
	default:
		return nil, fmt.Errorf("unsupported world type %q", worldType)
	}
	return append([]byte(nil), source...), nil
}
