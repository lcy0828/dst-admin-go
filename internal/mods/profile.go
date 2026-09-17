package mods

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

type RoomProfileWorldInput struct {
	WorldID string
	Name    string
	Master  bool
	Content []byte
}

type RoomProfileWorld struct {
	WorldID  string `json:"worldId"`
	Name     string `json:"name"`
	Master   bool   `json:"master"`
	Revision string `json:"revision"`
}

type RoomModWorldState struct {
	WorldID       string `json:"worldId"`
	Name          string `json:"name"`
	Master        bool   `json:"master"`
	Configured    bool   `json:"configured"`
	Enabled       bool   `json:"enabled"`
	Inherited     bool   `json:"inherited"`
	Difference    string `json:"difference,omitempty"`
	EntryRevision string `json:"entryRevision,omitempty"`
}

type RoomModProfileItem struct {
	ModID              string              `json:"modId"`
	ConfigurationMode  string              `json:"configurationMode"`
	ConfigurationMixed bool                `json:"configurationMixed"`
	DefaultConfigured  bool                `json:"defaultConfigured"`
	DefaultEnabled     bool                `json:"defaultEnabled"`
	DefaultRevision    string              `json:"defaultRevision,omitempty"`
	InheritedWorldIDs  []string            `json:"inheritedWorldIds"`
	ExceptionWorldIDs  []string            `json:"exceptionWorldIds"`
	Worlds             []RoomModWorldState `json:"worlds"`
}

type RoomModProfile struct {
	RoomID         string               `json:"roomId"`
	Revision       string               `json:"revision"`
	DefaultWorldID string               `json:"defaultWorldId"`
	Worlds         []RoomProfileWorld   `json:"worlds"`
	Items          []RoomModProfileItem `json:"items"`
}

// DeriveRoomModProfile treats the Master shard as the room default and derives
// exceptions from the normalized Lua entries observed on each runtime. It does
// not create a second configuration authority beside modoverrides.lua.
func DeriveRoomModProfile(roomID string, inputs []RoomProfileWorldInput) (RoomModProfile, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" || len(inputs) == 0 {
		return RoomModProfile{}, ErrInvalidRequest
	}
	worlds := append([]RoomProfileWorldInput(nil), inputs...)
	sort.SliceStable(worlds, func(left, right int) bool {
		if worlds[left].Master != worlds[right].Master {
			return worlds[left].Master
		}
		return strings.ToLower(worlds[left].Name) < strings.ToLower(worlds[right].Name)
	})
	seenWorlds := make(map[string]bool, len(worlds))
	snapshots := make(map[string]OverrideSnapshot, len(worlds))
	states := make(map[string]map[string]OverrideModState, len(worlds))
	profile := RoomModProfile{RoomID: roomID, DefaultWorldID: strings.TrimSpace(worlds[0].WorldID)}
	modIDs := make(map[string]bool)
	var revisionSource strings.Builder
	for _, world := range worlds {
		worldID := strings.TrimSpace(world.WorldID)
		if worldID == "" || seenWorlds[worldID] {
			return RoomModProfile{}, ErrInvalidRequest
		}
		seenWorlds[worldID] = true
		snapshot, err := InspectModOverride(world.Content)
		if err != nil {
			return RoomModProfile{}, err
		}
		snapshots[worldID] = snapshot
		states[worldID] = make(map[string]OverrideModState, len(snapshot.Mods))
		for _, state := range snapshot.Mods {
			states[worldID][state.ModID] = state
			modIDs[state.ModID] = true
		}
		profile.Worlds = append(profile.Worlds, RoomProfileWorld{
			WorldID: worldID, Name: world.Name, Master: world.Master, Revision: snapshot.Revision,
		})
		revisionSource.WriteString(worldID)
		revisionSource.WriteByte(0)
		revisionSource.WriteString(snapshot.Revision)
		revisionSource.WriteByte(0)
	}
	if profile.DefaultWorldID == "" {
		return RoomModProfile{}, ErrInvalidRequest
	}
	digest := sha256.Sum256([]byte(revisionSource.String()))
	profile.Revision = hex.EncodeToString(digest[:])

	orderedModIDs := make([]string, 0, len(modIDs))
	for modID := range modIDs {
		orderedModIDs = append(orderedModIDs, modID)
	}
	sort.Strings(orderedModIDs)
	defaultStates := states[profile.DefaultWorldID]
	for _, modID := range orderedModIDs {
		defaultState, defaultConfigured := defaultStates[modID]
		defaultEntryRevision := snapshots[profile.DefaultWorldID].EntryRevisions[modID]
		item := RoomModProfileItem{
			ModID: modID, ConfigurationMode: "shared", DefaultConfigured: defaultConfigured,
			DefaultEnabled:    defaultConfigured && defaultState.Enabled,
			DefaultRevision:   defaultEntryRevision,
			InheritedWorldIDs: []string{}, ExceptionWorldIDs: []string{}, Worlds: []RoomModWorldState{},
		}
		configurationRevision := ""
		for _, world := range profile.Worlds {
			state, configured := states[world.WorldID][modID]
			if configured {
				current := snapshots[world.WorldID].ConfigurationRevisions[modID]
				if configurationRevision != "" && current != configurationRevision {
					item.ConfigurationMixed = true
				}
				configurationRevision = current
			}
			entryRevision := snapshots[world.WorldID].EntryRevisions[modID]
			inherited := configured == defaultConfigured
			if inherited && configured {
				inherited = entryRevision != "" && entryRevision == defaultEntryRevision
			}
			difference := ""
			if !inherited {
				switch {
				case configured != defaultConfigured:
					difference = "configured"
				case state.Enabled != defaultState.Enabled:
					difference = "enabled"
				default:
					difference = "configuration"
				}
				item.ExceptionWorldIDs = append(item.ExceptionWorldIDs, world.WorldID)
			} else {
				item.InheritedWorldIDs = append(item.InheritedWorldIDs, world.WorldID)
			}
			item.Worlds = append(item.Worlds, RoomModWorldState{
				WorldID: world.WorldID, Name: world.Name, Master: world.Master,
				Configured: configured, Enabled: configured && state.Enabled, Inherited: inherited,
				Difference: difference, EntryRevision: entryRevision,
			})
		}
		profile.Items = append(profile.Items, item)
	}
	return profile, nil
}
