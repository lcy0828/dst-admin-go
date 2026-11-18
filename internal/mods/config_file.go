package mods

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"dont/internal/luaconfig"
	"dont/shared"

	lua "github.com/yuin/gopher-lua"
)

// NormalizeAndInspectModOverrides normalizes client-pasted modoverrides.lua
// content (stripping UTF-8 BOM and "KLEI     1" persistent headers, wrapping
// bare tables), validates literal AST safety and top-level mod entries, and
// preserves user comments/indentation unless format is true.
func NormalizeAndInspectModOverrides(rawContent string, format bool) ([]byte, OverrideSnapshot, []string, error) {
	normalized, err := luaconfig.NormalizeSource([]byte(rawContent), luaconfig.NormalizeOptions{
		MaxBytes:       maxLuaBytes,
		AllowBareTable: true,
	})
	if errors.Is(err, luaconfig.ErrFileTooLarge) {
		return nil, OverrideSnapshot{}, nil, ErrFileTooLarge
	}
	if err != nil {
		return nil, OverrideSnapshot{}, nil, wrapModSyntaxError(err)
	}
	if len(bytes.TrimSpace(normalized.Data)) == 0 {
		return nil, OverrideSnapshot{}, nil, &FileSyntaxError{
			File:    "modoverrides.lua",
			Line:    1,
			Message: "modoverrides.lua 不能为空；如需清空全部模组请填写 return {}",
		}
	}
	if err := luaconfig.ValidateNormalizedReturnTable(normalized, "modoverrides.lua", maxLuaBytes); err != nil {
		if errors.Is(err, luaconfig.ErrFileTooLarge) {
			return nil, OverrideSnapshot{}, nil, ErrFileTooLarge
		}
		return nil, OverrideSnapshot{}, nil, wrapModSyntaxError(err)
	}

	root, err := parseLuaTable(normalized.Data, "modoverrides.lua")
	if err != nil {
		return nil, OverrideSnapshot{}, nil, &FileSyntaxError{
			File:    "modoverrides.lua",
			Line:    1,
			Message: err.Error(),
		}
	}
	if err := validateModOverridesRoot(root); err != nil {
		return nil, OverrideSnapshot{}, nil, err
	}

	finalData := normalized.Data
	if format {
		rendered, renderErr := renderModOverride(modOverrideDocument{root: root})
		if renderErr != nil {
			return nil, OverrideSnapshot{}, nil, renderErr
		}
		finalData = rendered
	}

	doc := modOverrideDocument{
		root:     root,
		data:     append([]byte(nil), finalData...),
		mode:     0o640,
		exists:   true,
		revision: contentRevision(finalData),
	}
	notices := normalized.Notices
	if notices == nil {
		notices = []string{}
	}
	return finalData, overrideSnapshot(doc), notices, nil
}

func validateModOverridesRoot(root *luaNode) error {
	if root == nil || root.kind != lua.LTTable {
		return &FileSyntaxError{
			File:    "modoverrides.lua",
			Line:    1,
			Message: "modoverrides.lua 必须返回一个 Lua 配置表",
		}
	}
	if looksLikeWorldLevelData(root) {
		return &FileSyntaxError{
			File:    "modoverrides.lua",
			Line:    1,
			Message: "检测到世界配置（location / overrides），当前编辑的是模组配置 modoverrides.lua；请前往世界设置页面粘贴",
		}
	}
	for _, entry := range root.entries {
		if entry.key == nil || entry.key.kind != lua.LTString {
			return &FileSyntaxError{
				File:    "modoverrides.lua",
				Line:    1,
				Message: "modoverrides.lua 顶层键必须为模组字符串标识（如 [\"workshop-378160973\"]）",
			}
		}
		keyText := entry.key.text
		ref, valid := shared.ModReferenceFromKey(keyText)
		if !valid {
			return &FileSyntaxError{
				File:    "modoverrides.lua",
				Line:    1,
				Message: fmt.Sprintf("无效的模组键名 %q：创意工坊模组需为 workshop-<数字>，本地模组需为合法目录名", keyText),
			}
		}
		if entry.value == nil || entry.value.kind != lua.LTTable {
			return &FileSyntaxError{
				File:    "modoverrides.lua",
				Line:    1,
				Message: fmt.Sprintf("模组 %q 的值必须为包含 enabled / configuration_options 的表", keyText),
			}
		}
		if enabledNode, hasEnabled := entry.value.stringEntry("enabled"); hasEnabled && enabledNode.kind != lua.LTBool {
			return &FileSyntaxError{
				File:    "modoverrides.lua",
				Line:    1,
				Message: fmt.Sprintf("模组 %q 的 enabled 字段必须为布尔值（true 或 false）", keyText),
			}
		}
		if optionsNode, hasOptions := entry.value.stringEntry("configuration_options"); hasOptions && optionsNode.kind != lua.LTTable {
			return &FileSyntaxError{
				File:    "modoverrides.lua",
				Line:    1,
				Message: fmt.Sprintf("模组 %q 的 configuration_options 字段必须为表", keyText),
			}
		}
		if ref.Source == "local" && len(entry.value.entries) > 0 {
			_, hasEnabled := entry.value.stringEntry("enabled")
			_, hasOptions := entry.value.stringEntry("configuration_options")
			if !hasEnabled && !hasOptions {
				return &FileSyntaxError{
					File:    "modoverrides.lua",
					Line:    1,
					Message: fmt.Sprintf("本地模组 %q 的配置表需包含 enabled 或 configuration_options 字段", keyText),
				}
			}
		}
	}
	return nil
}

func looksLikeWorldLevelData(root *luaNode) bool {
	if root == nil {
		return false
	}
	if overrides, ok := root.stringEntry("overrides"); ok && overrides.kind == lua.LTTable {
		if loc, hasLoc := root.stringEntry("location"); hasLoc && loc.kind == lua.LTString {
			return true
		}
		// Even without location, if overrides is the sole or accompanied by worldgen_preset
		if len(root.entries) == 1 {
			return true
		}
		if _, hasPreset := root.stringEntry("worldgen_preset"); hasPreset {
			return true
		}
	}
	return false
}

// DiffModOverridesAcrossWorlds compares the target OverrideSnapshot against
// the current OverrideSnapshots of all selected worlds.
func DiffModOverridesAcrossWorlds(beforeByWorld map[string]OverrideSnapshot, selectedWorldIDs []string, after OverrideSnapshot) []ConfigurationFileModChange {
	afterMods := make(map[string]OverrideModState, len(after.Mods))
	allModIDs := make(map[string]bool, len(after.Mods))
	for _, m := range after.Mods {
		afterMods[m.ModID] = m
		allModIDs[m.ModID] = true
	}

	beforeModsByWorld := make(map[string]map[string]OverrideModState, len(selectedWorldIDs))
	for _, wid := range selectedWorldIDs {
		snap := beforeByWorld[wid]
		byID := make(map[string]OverrideModState, len(snap.Mods))
		for _, m := range snap.Mods {
			byID[m.ModID] = m
			allModIDs[m.ModID] = true
		}
		beforeModsByWorld[wid] = byID
	}

	sortedIDs := make([]string, 0, len(allModIDs))
	for id := range allModIDs {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)

	changes := make([]ConfigurationFileModChange, 0, len(sortedIDs))
	for _, modID := range sortedIDs {
		ref, _ := ParseReference(modID)
		afterState, inAfter := afterMods[modID]
		if !inAfter {
			existedBefore := false
			for _, wid := range selectedWorldIDs {
				if _, ok := beforeModsByWorld[wid][modID]; ok {
					existedBefore = true
					break
				}
			}
			if existedBefore {
				changes = append(changes, ConfigurationFileModChange{
					ModID:         ref.ID,
					Source:        ref.Source,
					DirectoryName: ref.DirectoryName,
					Enabled:       false,
					Operation:     "removed",
				})
			}
			continue
		}

		missingInAnyWorld := false
		enabledChangedInAnyWorld := false
		optionsChangedInAnyWorld := false
		afterEntryRev := after.EntryRevisions[modID]
		for _, wid := range selectedWorldIDs {
			beforeState, ok := beforeModsByWorld[wid][modID]
			if !ok {
				missingInAnyWorld = true
				continue
			}
			if beforeState.Enabled != afterState.Enabled {
				enabledChangedInAnyWorld = true
			}
			if beforeByWorld[wid].EntryRevisions[modID] != afterEntryRev {
				optionsChangedInAnyWorld = true
			}
		}

		op := ""
		switch {
		case missingInAnyWorld:
			op = "added"
		case enabledChangedInAnyWorld:
			op = "enabled_changed"
		case optionsChangedInAnyWorld:
			op = "options_changed"
		}
		if op == "" {
			continue
		}
		changes = append(changes, ConfigurationFileModChange{
			ModID:         ref.ID,
			Source:        ref.Source,
			DirectoryName: ref.DirectoryName,
			Enabled:       afterState.Enabled,
			Operation:     op,
		})
	}
	return changes
}

func wrapModSyntaxError(err error) error {
	var syntaxErr *luaconfig.SyntaxError
	if errors.As(err, &syntaxErr) {
		msg := strings.TrimSpace(syntaxErr.Message)
		return &FileSyntaxError{
			File:    "modoverrides.lua",
			Line:    syntaxErr.Line,
			Column:  syntaxErr.Column,
			Message: msg,
		}
	}
	return &FileSyntaxError{
		File:    "modoverrides.lua",
		Message: err.Error(),
	}
}
