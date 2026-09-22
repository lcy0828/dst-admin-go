package entitycatalog

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// Display metadata only, extracted from Klei's registered prefabs and scrapbook.
// It contains no game scripts, constructors or textures. Images remain optional.
//
//go:embed data/vanilla.json
var vanillaDirectory []byte

type DirectoryResult struct {
	GameVersion string   `json:"gameVersion"`
	Items       []Entity `json:"items"`
}

var directoryOnce sync.Once
var directory DirectoryResult

// Directory is independent of the artwork pack and the running game's state.
// Registration/operation support must still be checked by the target world.
func Directory() DirectoryResult {
	directoryOnce.Do(func() {
		var data struct {
			GameVersion string      `json:"gameVersion"`
			Items       [][4]string `json:"items"`
		}
		if err := json.Unmarshal(vanillaDirectory, &data); err != nil {
			panic("invalid embedded entity directory: " + err.Error())
		}
		directory.GameVersion = data.GameVersion
		commonByID := make(map[string]Entity, len(commonEntities))
		for _, item := range commonEntities {
			commonByID[item.ID] = item
		}
		for _, row := range data.Items {
			item := Entity{ID: row[0], Key: "dst:" + row[0], Namespace: "dst", NameZhCN: row[1], NameEn: row[2], Type: row[3], Category: row[3], Source: "builtin", Capabilities: []Capability{CapabilityGive, CapabilitySpawn, CapabilityRemove}}
			if common, ok := commonByID[item.ID]; ok {
				item.Common, item.NameZhCN, item.NameEn = true, common.NameZhCN, common.NameEn
			}
			directory.Items = append(directory.Items, item)
		}
	})
	return directory
}
