package mods

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

type configPlan struct {
	configuration ModConfiguration
	preview       ConfigPreview
	mutation      fileMutation
}

func (s *Service) ConfigurationFile(roomID, worldID string) (ConfigurationFile, error) {
	_, release, err := s.acquireRoom(context.Background(), roomID)
	if err != nil {
		return ConfigurationFile{}, err
	}
	defer release()
	_, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ConfigurationFile{}, err
	}
	world, err := s.rooms.World(roomID, worldID)
	if err != nil {
		return ConfigurationFile{}, err
	}
	worldPath, err := safeDirectory(roomPath, world.DirectoryName)
	if err != nil {
		return ConfigurationFile{}, err
	}
	data, _, exists, err := readModFile(filepath.Join(worldPath, "modoverrides.lua"), true)
	if err != nil {
		return ConfigurationFile{}, err
	}
	return ConfigurationFile{
		RoomID: roomID, WorldID: worldID, FileName: "modoverrides.lua",
		Content: string(data), Exists: exists, Revision: contentRevision(data), ReadAt: s.now().UTC(),
	}, nil
}

func (s *Service) Configuration(ctx context.Context, roomID, worldID, modID string) (ModConfiguration, error) {
	if !validModID(modID) {
		return ModConfiguration{}, ErrInvalidModID
	}
	ctx, release, err := s.acquireRoom(ctx, roomID)
	if err != nil {
		return ModConfiguration{}, err
	}
	defer release()
	configuration, _, _, err := s.loadConfiguration(ctx, roomID, worldID, modID)
	return configuration, err
}

func (s *Service) PreviewConfiguration(ctx context.Context, roomID, worldID, modID string, request ConfigUpdateRequest) (ConfigPreview, error) {
	if !validModID(modID) {
		return ConfigPreview{}, ErrInvalidModID
	}
	ctx, release, err := s.acquireRoom(ctx, roomID)
	if err != nil {
		return ConfigPreview{}, err
	}
	defer release()
	plan, err := s.planConfiguration(ctx, roomID, worldID, modID, request)
	return plan.preview, err
}

func (s *Service) ApplyConfiguration(ctx context.Context, jobID, roomID, worldID, modID string, request ConfigUpdateRequest) (ConfigApplyResult, error) {
	if !validModID(modID) {
		return ConfigApplyResult{}, ErrInvalidModID
	}
	if err := s.requireLocalRoom(roomID); err != nil {
		return ConfigApplyResult{}, err
	}
	ctx, release, err := s.acquireRoom(ctx, roomID)
	if err != nil {
		return ConfigApplyResult{}, err
	}
	defer release()
	plan, err := s.planConfiguration(ctx, roomID, worldID, modID, request)
	if err != nil {
		return ConfigApplyResult{}, err
	}
	room, _, err := s.resolveRoom(roomID)
	if err != nil {
		return ConfigApplyResult{}, err
	}
	backup, err := s.protectionBackup(ctx, room, "Mod 配置", jobID)
	if err != nil {
		return ConfigApplyResult{}, err
	}
	if err := applyFileMutations([]fileMutation{plan.mutation}); err != nil {
		return ConfigApplyResult{}, err
	}
	return ConfigApplyResult{
		Revision: plan.preview.NextRevision, Changes: plan.preview.Changes,
		Warnings: plan.preview.Warnings, ProtectionBackupID: backup.ID,
	}, nil
}

func (s *Service) planConfiguration(ctx context.Context, roomID, worldID, modID string, request ConfigUpdateRequest) (configPlan, error) {
	configuration, document, path, err := s.loadConfiguration(ctx, roomID, worldID, modID)
	if err != nil {
		return configPlan{}, err
	}
	if strings.TrimSpace(request.ExpectedRevision) == "" {
		return configPlan{}, &FieldError{Fields: map[string]string{"expectedRevision": "必须提供当前 revision"}}
	}
	if request.ExpectedRevision != configuration.Revision {
		return configPlan{}, &RevisionConflictError{CurrentRevision: configuration.Revision}
	}
	if len(request.Patch) > 500 {
		return configPlan{}, &FieldError{Fields: map[string]string{"patch": "一次最多修改 500 个配置项"}}
	}
	entry, ok := document.mod(modID)
	if !ok {
		return configPlan{}, ErrModNotConfigured
	}
	fieldByKey := make(map[string]ConfigField, len(configuration.Fields))
	for _, field := range configuration.Fields {
		fieldByKey[field.Key] = field
	}
	changes := make([]ConfigChange, 0, len(request.Patch)+1)
	currentEnabled := modEnabled(entry)
	if currentEnabled != request.Enabled {
		changes = append(changes, ConfigChange{Path: "enabled", Label: "启用状态", Before: currentEnabled, After: request.Enabled, Operation: "replace"})
		entry.setStringEntry("enabled", boolNode(request.Enabled))
	}
	options, _ := entry.stringEntry("configuration_options")
	if options == nil || options.kind != lua.LTTable {
		options = tableNode()
		entry.setStringEntry("configuration_options", options)
	}
	keys := make([]string, 0, len(request.Patch))
	for key := range request.Patch {
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
		node, remove, decodeErr := rawJSONNode(request.Patch[key])
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
			if beforeNode == nil {
				continue
			}
			options.setStringEntry(key, nil)
			changes = append(changes, ConfigChange{Path: "configuration_options." + key, Label: field.Label, Before: before, Operation: "remove"})
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
		return configPlan{}, &FieldError{Fields: fieldErrors}
	}
	if len(changes) == 0 {
		return configPlan{}, ErrNoChanges
	}
	next, err := renderModOverride(document)
	if err != nil {
		return configPlan{}, err
	}
	preview := ConfigPreview{
		Revision: configuration.Revision, NextRevision: contentRevision(next), Changes: changes,
		Warnings: append([]string(nil), configuration.Warnings...), RawPreserved: true,
	}
	return configPlan{
		configuration: configuration,
		preview:       preview,
		mutation: fileMutation{
			path: path, data: next,
			previous: fileSnapshot{data: document.data, mode: document.mode, exists: document.exists},
		},
	}, nil
}

func (s *Service) loadConfiguration(ctx context.Context, roomID, worldID, modID string) (ModConfiguration, modOverrideDocument, string, error) {
	_, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ModConfiguration{}, modOverrideDocument{}, "", err
	}
	world, err := s.rooms.World(roomID, worldID)
	if err != nil {
		return ModConfiguration{}, modOverrideDocument{}, "", err
	}
	worldPath, err := safeDirectory(roomPath, world.DirectoryName)
	if err != nil {
		return ModConfiguration{}, modOverrideDocument{}, "", err
	}
	path := filepath.Join(worldPath, "modoverrides.lua")
	document, err := loadModOverride(path)
	if err != nil {
		return ModConfiguration{}, modOverrideDocument{}, "", err
	}
	entry, ok := document.mod(modID)
	if !ok {
		return ModConfiguration{}, modOverrideDocument{}, "", ErrModNotConfigured
	}
	modInfoPath := filepath.Join(s.downloadedPath(modID), "modinfo.lua")
	if _, err := safeRegularFile(modInfoPath); err != nil {
		if os.IsNotExist(err) {
			return ModConfiguration{}, modOverrideDocument{}, "", ErrModInfoUnavailable
		}
		return ModConfiguration{}, modOverrideDocument{}, "", fmt.Errorf("inspect modinfo.lua: %w", err)
	}
	parsed, err := s.parser.Parse(ctx, modID, modInfoPath)
	if err != nil {
		return ModConfiguration{}, modOverrideDocument{}, "", fmt.Errorf("parse modinfo.lua: %w", err)
	}
	fields := configurationFields(parsed.Values["configuration_options"])
	known := make(map[string]ConfigField, len(fields))
	for _, field := range fields {
		known[field.Key] = field
	}
	values := make(map[string]interface{}, len(fields))
	overrides := make(map[string]interface{})
	for _, field := range fields {
		values[field.Key] = field.DefaultValue
	}
	unknown := make(map[string]interface{})
	if options, _ := entry.stringEntry("configuration_options"); options != nil && options.kind == lua.LTTable {
		for _, option := range options.entries {
			key, keyErr := nodeKeyString(option.key)
			if keyErr != nil {
				continue
			}
			value, valueErr := nodeToInterface(option.value)
			if valueErr != nil {
				continue
			}
			if _, exists := known[key]; exists {
				values[key] = value
				overrides[key] = value
			} else {
				unknown[key] = value
			}
		}
	}
	return ModConfiguration{
		Revision: document.revision, RoomID: roomID, WorldID: worldID, ModID: modID,
		Enabled: modEnabled(entry), Parser: parsed.Parser, FallbackUsed: parsed.FallbackUsed,
		FallbackReason: parsed.FallbackReason, Warnings: nonNilStrings(parsed.Warnings),
		RawPreserved: true, SchemaVersion: "1", Fields: fields, Values: values, Overrides: overrides, UnknownValues: unknown,
	}, document, path, nil
}

func configurationFields(raw interface{}) []ConfigField {
	items, ok := raw.([]interface{})
	if !ok {
		return []ConfigField{}
	}
	result := make([]ConfigField, 0, len(items))
	seen := make(map[string]bool)
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]interface{})
		if !ok {
			continue
		}
		key, _ := item["name"].(string)
		key = strings.TrimSpace(key)
		if key == "" || len(key) > 256 || seen[key] {
			continue
		}
		seen[key] = true
		label, _ := item["label"].(string)
		if strings.TrimSpace(label) == "" {
			label = key
		}
		description, _ := item["hover"].(string)
		field := ConfigField{Key: key, Label: label, Description: description, DefaultValue: normalizeConfigValue(item["default"]), Options: []ConfigOption{}}
		if rawOptions, ok := item["options"].([]interface{}); ok {
			for _, rawOption := range rawOptions {
				option, ok := rawOption.(map[string]interface{})
				if !ok {
					continue
				}
				value, exists := option["data"]
				if !exists || !supportedConfigValue(value, 0) {
					continue
				}
				optionLabel, _ := option["description"].(string)
				if optionLabel == "" {
					optionLabel = fmt.Sprint(value)
				}
				hint, _ := option["hover"].(string)
				field.Options = append(field.Options, ConfigOption{Value: normalizeConfigValue(value), Label: optionLabel, Hint: hint})
			}
		}
		field.Type = inferConfigType(field.DefaultValue, field.Options)
		result = append(result, field)
	}
	return result
}

func inferConfigType(value interface{}, options []ConfigOption) string {
	if len(options) > 0 {
		if _, ok := value.(bool); ok && len(options) == 2 {
			return "boolean"
		}
		return "select"
	}
	switch value.(type) {
	case bool:
		return "boolean"
	case float64, json.Number:
		return "number"
	case string:
		return "text"
	default:
		return "json"
	}
}

func validConfigurationValue(field ConfigField, value interface{}) bool {
	if !supportedConfigValue(value, 0) {
		return false
	}
	if len(field.Options) > 0 {
		for _, option := range field.Options {
			if reflect.DeepEqual(normalizeConfigValue(option.Value), normalizeConfigValue(value)) {
				return true
			}
		}
		return false
	}
	switch field.Type {
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := normalizeConfigValue(value).(float64)
		return ok
	case "text":
		_, ok := value.(string)
		return ok
	default:
		return true
	}
}

func supportedConfigValue(value interface{}, depth int) bool {
	if depth > 32 {
		return false
	}
	switch typed := value.(type) {
	case nil, bool, string, float64, json.Number:
		return true
	case []interface{}:
		for _, item := range typed {
			if !supportedConfigValue(item, depth+1) {
				return false
			}
		}
		return true
	case map[string]interface{}:
		for key, item := range typed {
			if key == "" || !supportedConfigValue(item, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func normalizeConfigValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		if err == nil {
			return number
		}
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index, item := range typed {
			result[index] = normalizeConfigValue(item)
		}
		return result
	case map[string]interface{}:
		result := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			result[key] = normalizeConfigValue(item)
		}
		return result
	}
	return value
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
