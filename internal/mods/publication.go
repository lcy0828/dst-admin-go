package mods

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

type OverrideAction string

const (
	OverrideActionAdd       OverrideAction = "add"
	OverrideActionEnable    OverrideAction = "enable"
	OverrideActionRemove    OverrideAction = "remove"
	OverrideActionConfigure OverrideAction = "configure"
)

type OverrideModState struct {
	ModID   string `json:"modId"`
	Enabled bool   `json:"enabled"`
}

type OverrideSnapshot struct {
	Revision string             `json:"revision"`
	Mods     []OverrideModState `json:"mods"`
}

type OverrideMutation struct {
	Action           OverrideAction
	ModIDs           []string
	ModID            string
	Enabled          bool
	ExpectedRevision string
	Patch            map[string]json.RawMessage
	Fields           []ConfigField
}

type OverrideMutationResult struct {
	Content      []byte             `json:"content"`
	Revision     string             `json:"revision"`
	NextRevision string             `json:"nextRevision"`
	Mods         []OverrideModState `json:"mods"`
	Changes      []ConfigChange     `json:"changes"`
}

func InspectModOverride(content []byte) (OverrideSnapshot, error) {
	document, err := parseModOverrideContent(content, 0o640, len(content) > 0, "modoverrides.lua")
	if err != nil {
		return OverrideSnapshot{}, err
	}
	return overrideSnapshot(document), nil
}

// MutateModOverride applies a typed change to Lua bytes without reading or
// writing a host path. Callers can therefore use the same revision-checked AST
// logic for local and remotely placed worlds.
func MutateModOverride(content []byte, mutation OverrideMutation) (OverrideMutationResult, error) {
	document, err := parseModOverrideContent(content, 0o640, len(content) > 0, "modoverrides.lua")
	if err != nil {
		return OverrideMutationResult{}, err
	}
	if strings.TrimSpace(mutation.ExpectedRevision) == "" {
		return OverrideMutationResult{}, &FieldError{Fields: map[string]string{"expectedRevision": "必须提供当前 revision"}}
	}
	if mutation.ExpectedRevision != document.revision {
		return OverrideMutationResult{}, &RevisionConflictError{CurrentRevision: document.revision}
	}
	changes := make([]ConfigChange, 0)
	switch mutation.Action {
	case OverrideActionAdd:
		ids := uniqueModIDs(mutation.ModIDs)
		if len(ids) == 0 {
			return OverrideMutationResult{}, ErrInvalidRequest
		}
		for _, modID := range ids {
			if !validModID(modID) {
				return OverrideMutationResult{}, ErrInvalidModID
			}
			entry, exists := document.mod(modID)
			beforeEnabled := exists && entry != nil && modEnabled(entry)
			if exists && entry != nil && entry.kind.String() == "table" && beforeEnabled == mutation.Enabled {
				if _, configured := modConfiguration(entry); configured {
					continue
				}
			}
			ensureModEntry(&document, modID, mutation.Enabled)
			changes = append(changes, ConfigChange{Path: "workshop-" + modID, Label: modID, Before: exists, After: true, Operation: "add"})
		}
	case OverrideActionEnable:
		entry, validationErr := requireOverrideEntry(document, mutation.ModID)
		if validationErr != nil {
			return OverrideMutationResult{}, validationErr
		}
		before := modEnabled(entry)
		if before != mutation.Enabled {
			entry.setStringEntry("enabled", boolNode(mutation.Enabled))
			changes = append(changes, ConfigChange{Path: "enabled", Label: "启用状态", Before: before, After: mutation.Enabled, Operation: "replace"})
		}
	case OverrideActionRemove:
		if !validModID(mutation.ModID) {
			return OverrideMutationResult{}, ErrInvalidModID
		}
		if _, ok := document.mod(mutation.ModID); ok {
			document.root.setStringEntry("workshop-"+mutation.ModID, nil)
			changes = append(changes, ConfigChange{Path: "workshop-" + mutation.ModID, Label: mutation.ModID, Before: true, Operation: "remove"})
		}
	case OverrideActionConfigure:
		entry, validationErr := requireOverrideEntry(document, mutation.ModID)
		if validationErr != nil {
			return OverrideMutationResult{}, validationErr
		}
		configurationChanges, patchErr := applyOverrideConfiguration(entry, mutation)
		if patchErr != nil {
			return OverrideMutationResult{}, patchErr
		}
		changes = append(changes, configurationChanges...)
	default:
		return OverrideMutationResult{}, ErrInvalidRequest
	}
	if len(changes) == 0 {
		return OverrideMutationResult{}, ErrNoChanges
	}
	next, err := renderModOverride(document)
	if err != nil {
		return OverrideMutationResult{}, err
	}
	nextDocument, err := parseModOverrideContent(next, 0o640, true, "modoverrides.lua")
	if err != nil {
		return OverrideMutationResult{}, err
	}
	return OverrideMutationResult{
		Content: next, Revision: document.revision, NextRevision: contentRevision(next),
		Mods: overrideSnapshot(nextDocument).Mods, Changes: changes,
	}, nil
}

func requireOverrideEntry(document modOverrideDocument, modID string) (*luaNode, error) {
	if !validModID(modID) {
		return nil, ErrInvalidModID
	}
	entry, ok := document.mod(modID)
	if !ok || entry == nil {
		return nil, ErrModNotConfigured
	}
	return entry, nil
}

func applyOverrideConfiguration(entry *luaNode, mutation OverrideMutation) ([]ConfigChange, error) {
	if len(mutation.Patch) > 500 {
		return nil, &FieldError{Fields: map[string]string{"patch": "一次最多修改 500 个配置项"}}
	}
	fieldByKey := make(map[string]ConfigField, len(mutation.Fields))
	for _, field := range mutation.Fields {
		fieldByKey[field.Key] = field
	}
	changes := make([]ConfigChange, 0, len(mutation.Patch)+1)
	currentEnabled := modEnabled(entry)
	if currentEnabled != mutation.Enabled {
		changes = append(changes, ConfigChange{Path: "enabled", Label: "启用状态", Before: currentEnabled, After: mutation.Enabled, Operation: "replace"})
		entry.setStringEntry("enabled", boolNode(mutation.Enabled))
	}
	options, _ := entry.stringEntry("configuration_options")
	if options == nil || options.kind.String() != "table" {
		options = tableNode()
		entry.setStringEntry("configuration_options", options)
	}
	keys := make([]string, 0, len(mutation.Patch))
	for key := range mutation.Patch {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fieldErrors := make(map[string]string)
	for _, key := range keys {
		field, exists := fieldByKey[key]
		if !exists {
			fieldErrors["patch."+key] = "该字段不在 Mod 配置 schema 中"
			continue
		}
		node, remove, decodeErr := rawJSONNode(mutation.Patch[key])
		if decodeErr != nil {
			fieldErrors["patch."+key] = "JSON 值无效"
			continue
		}
		var next interface{}
		if !remove {
			next, decodeErr = nodeToInterface(node)
			if decodeErr != nil || !validConfigurationValue(field, next) {
				fieldErrors["patch."+key] = "值不符合 Mod 声明的类型或选项"
				continue
			}
		}
		beforeNode, _ := options.stringEntry(key)
		var before interface{}
		if beforeNode != nil {
			before, _ = nodeToInterface(beforeNode)
		}
		if remove {
			if beforeNode != nil {
				options.setStringEntry(key, nil)
				changes = append(changes, ConfigChange{Path: "configuration_options." + key, Label: field.Label, Before: before, Operation: "remove"})
			}
			continue
		}
		if reflect.DeepEqual(normalizeConfigValue(before), normalizeConfigValue(next)) {
			continue
		}
		operation := "replace"
		if beforeNode == nil {
			operation = "add"
		}
		options.setStringEntry(key, node)
		changes = append(changes, ConfigChange{Path: "configuration_options." + key, Label: field.Label, Before: before, After: next, Operation: operation})
	}
	if len(fieldErrors) > 0 {
		return nil, &FieldError{Fields: fieldErrors}
	}
	return changes, nil
}

func overrideSnapshot(document modOverrideDocument) OverrideSnapshot {
	mods := make([]OverrideModState, 0)
	for _, entry := range document.root.entries {
		if entry.key.kind.String() != "string" || !strings.HasPrefix(entry.key.text, "workshop-") {
			continue
		}
		modID := strings.TrimPrefix(entry.key.text, "workshop-")
		if validModID(modID) {
			mods = append(mods, OverrideModState{ModID: modID, Enabled: modEnabled(entry.value)})
		}
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].ModID < mods[j].ModID })
	return OverrideSnapshot{Revision: document.revision, Mods: mods}
}
