package configuration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"dont/internal/luaconfig"
	"dont/internal/roomops"
	"dont/internal/rooms"

	"github.com/go-ini/ini"
	lua "github.com/yuin/gopher-lua"
)

const maxWorldFileBytes = int64(240 * 1024)

func (s *Service) WorldFiles(roomID, worldID string) (WorldFiles, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.WorldFilesContext(ctx, roomID, worldID)
}

func (s *Service) WorldFilesContext(ctx context.Context, roomID, worldID string) (WorldFiles, error) {
	_, world, _, document, _, err := s.worldDocument(ctx, roomID, worldID, true)
	if err != nil {
		return WorldFiles{}, err
	}
	cfg, err := worldConfigFromDocument(document)
	if err != nil {
		return WorldFiles{}, err
	}
	return WorldFiles{
		Revision:          document.revision,
		WorldType:         canonicalWorldType(world, document),
		ServerIni:         string(document.serverData),
		Leveldataoverride: string(document.luaData),
		Server:            cfg.Server,
		Overrides:         cfg.Overrides,
		UnknownFieldCount: cfg.UnknownFieldCount,
		ModifiedAt:        document.modified,
		Sync:              document.sync,
	}, nil
}

func (s *Service) PreviewWorldFiles(roomID, worldID string, request WorldFilesUpdateRequest) (WorldFilesPreview, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.PreviewWorldFilesContext(ctx, roomID, worldID, request)
}

func (s *Service) PreviewWorldFilesContext(ctx context.Context, roomID, worldID string, request WorldFilesUpdateRequest) (WorldFilesPreview, error) {
	_, world, _, document, _, err := s.worldDocument(ctx, roomID, worldID, false)
	if err != nil {
		return WorldFilesPreview{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return WorldFilesPreview{}, err
	}
	_, _, preview, err := renderWorldFilesDocument(world, document, request)
	return preview, err
}

func (s *Service) ApplyWorldFiles(ctx context.Context, jobID, roomID, worldID string, request WorldFilesUpdateRequest) (ApplyResult, error) {
	request.AllowEmptyChanges = false
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	defer release()

	_, world, worldPath, document, routed, err := s.worldDocument(ctx, roomID, worldID, false)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return ApplyResult{}, err
	}
	if _, _, _, err := renderWorldFilesDocument(world, document, request); err != nil {
		return ApplyResult{}, err
	}

	_, latestWorld, _, latest, latestRouted, err := s.worldDocument(ctx, roomID, worldID, false)
	if err != nil {
		return ApplyResult{}, err
	}
	if latestRouted != routed {
		return ApplyResult{}, &RevisionConflictError{CurrentRevision: latest.revision}
	}
	if err := checkRevision(request.ExpectedRevision, latest.revision); err != nil {
		return ApplyResult{}, err
	}
	serverData, luaData, latestPreview, err := renderWorldFilesDocument(latestWorld, latest, request)
	if err != nil {
		return ApplyResult{}, err
	}

	previous := map[string]fileSnapshot{
		"server.ini":            {data: latest.serverData, mode: latest.serverMode, exists: true},
		"leveldataoverride.lua": {data: latest.luaData, mode: latest.luaMode, exists: true},
	}
	if routed {
		if s.publisher == nil {
			return ApplyResult{}, wrapApplyError("world publication", errors.New("configuration publisher is unavailable"))
		}
		publicationFiles := worldPublicationFiles(serverData, latest.serverMode, luaData, latest.luaMode)
		published, publishErr := s.publish(ctx, PublicationRequest{
			RoomID: roomID, WorldID: worldID, Scope: PublicationWorld,
			Files: []string{"server.ini", "leveldataoverride.lua"}, IncludeLocal: true,
			Payload: publicationFiles,
			ExpectedFiles: map[string]string{
				"server.ini":            configurationFileDigest(latest.serverData),
				"leveldataoverride.lua": configurationFileDigest(latest.luaData),
			},
		})
		if publishErr != nil {
			return ApplyResult{}, wrapApplyError("world publication", publishErr)
		}
		sync := SyncState{Status: "synced", Source: "runtime-disk", ObservedRevision: latestPreview.NextRevision}
		return ApplyResult{Revision: latestPreview.NextRevision, Changes: latestPreview.Changes, PublishedTargets: published, Sync: sync}, nil
	}

	if err := atomicWriteSet(worldPath, previous, []fileWrite{{name: "server.ini", data: serverData}, {name: "leveldataoverride.lua", data: luaData}}); err != nil {
		return ApplyResult{}, wrapApplyError("world", err)
	}
	published, err := s.publish(ctx, PublicationRequest{
		RoomID:  roomID,
		WorldID: worldID,
		Scope:   PublicationWorld,
		Files:   []string{"server.ini", "leveldataoverride.lua"},
		Payload: worldPublicationFiles(serverData, latest.serverMode, luaData, latest.luaMode),
	})
	if err != nil {
		rollbackErr := rollbackWrites(worldPath, previous, []string{"server.ini", "leveldataoverride.lua"})
		return ApplyResult{}, wrapApplyError("world publication", errors.Join(err, rollbackErr))
	}
	observed, err := loadWorldDocument(worldPath)
	if err != nil || observed.revision != latestPreview.NextRevision {
		return ApplyResult{}, wrapApplyError("world verification", errors.Join(err, &RevisionConflictError{CurrentRevision: observed.revision}))
	}
	observedAt := observed.modified.UTC()
	return ApplyResult{
		Revision:         observed.revision,
		Changes:          latestPreview.Changes,
		PublishedTargets: published,
		Sync: SyncState{
			Status: "synced", Source: "runtime-disk", ObservedRevision: observed.revision, ObservedAt: &observedAt,
		},
	}, nil
}

func renderWorldFilesDocument(world rooms.World, document worldDocument, request WorldFilesUpdateRequest) ([]byte, []byte, WorldFilesPreview, error) {
	notices := make([]string, 0, 2)

	// 1. Resolve and validate server.ini
	serverData := append([]byte(nil), document.serverData...)
	if request.ServerIni != nil {
		normalizedIni, err := normalizeIniSource([]byte(*request.ServerIni), maxWorldFileBytes)
		if err != nil {
			return nil, nil, WorldFilesPreview{}, err
		}
		serverData = normalizedIni
	}

	serverConfig, err := ini.Load(serverData)
	if err != nil {
		return nil, nil, WorldFilesPreview{}, &FileSyntaxError{
			File:    "server.ini",
			Message: fmt.Sprintf("server.ini 语法无效: %v", err),
		}
	}
	serverValues := serverValuesFromIni(serverConfig)
	if request.ServerIni != nil && serverValues.IsMaster != document.serverValues.IsMaster {
		return nil, nil, WorldFilesPreview{}, &FieldError{
			Fields: map[string]string{"server.isMaster": "不能在配置文件中直接翻转当前世界的主从分片角色（is_master）"},
		}
	}

	formatAll := request.Format && request.ServerIni == nil && request.Leveldataoverride == nil
	if request.Server != nil {
		nextServer := *request.Server
		if nextServer.IsMaster != document.serverValues.IsMaster {
			return nil, nil, WorldFilesPreview{}, &FieldError{
				Fields: map[string]string{"server.isMaster": "不能直接翻转当前世界的主从分片角色（is_master）"},
			}
		}
		if err := validateWorldServer(nextServer); err != nil {
			return nil, nil, WorldFilesPreview{}, err
		}
		if applyServerValuesToIni(serverConfig, serverValues, nextServer) || (request.Format && request.ServerIni != nil) || formatAll {
			var buf bytes.Buffer
			if _, err := serverConfig.WriteTo(&buf); err != nil {
				return nil, nil, WorldFilesPreview{}, err
			}
			serverData = buf.Bytes()
			serverValues = nextServer
		}
	} else if request.ServerIni != nil || formatAll {
		if err := validateWorldServer(serverValues); err != nil {
			return nil, nil, WorldFilesPreview{}, err
		}
		if (request.Format && request.ServerIni != nil) || formatAll {
			var buf bytes.Buffer
			if _, err := serverConfig.WriteTo(&buf); err != nil {
				return nil, nil, WorldFilesPreview{}, err
			}
			serverData = buf.Bytes()
		}
	}


	// 2. Resolve and validate leveldataoverride.lua
	luaData := append([]byte(nil), document.luaData...)
	if request.Leveldataoverride != nil {
		normalizedLua, normErr := luaconfig.NormalizeSource([]byte(*request.Leveldataoverride), luaconfig.NormalizeOptions{
			MaxBytes:       maxWorldFileBytes,
			AllowBareTable: true,
		})
		if errors.Is(normErr, luaconfig.ErrFileTooLarge) {
			return nil, nil, WorldFilesPreview{}, ErrFileTooLarge
		}
		if normErr != nil {
			return nil, nil, WorldFilesPreview{}, wrapLuaSyntaxError("leveldataoverride.lua", normErr)
		}
		notices = append(notices, normalizedLua.Notices...)
		if len(bytes.TrimSpace(normalizedLua.Data)) == 0 {
			return nil, nil, WorldFilesPreview{}, &FileSyntaxError{
				File:    "leveldataoverride.lua",
				Line:    1,
				Message: "leveldataoverride.lua 不能为空",
			}
		}
		if valErr := luaconfig.ValidateNormalizedReturnTable(normalizedLua, "leveldataoverride.lua", maxWorldFileBytes); valErr != nil {
			if errors.Is(valErr, luaconfig.ErrFileTooLarge) {
				return nil, nil, WorldFilesPreview{}, ErrFileTooLarge
			}
			return nil, nil, WorldFilesPreview{}, wrapLuaSyntaxError("leveldataoverride.lua", valErr)
		}
		luaData = normalizedLua.Data
	}

	root, err := parseLuaReturnTable(luaData)
	if err != nil {
		return nil, nil, WorldFilesPreview{}, &FileSyntaxError{
			File:    "leveldataoverride.lua",
			Line:    1,
			Message: err.Error(),
		}
	}
	overrides, ok := root.stringEntry("overrides")
	if !ok || overrides.kind != lua.LTTable {
		if looksLikeModOverrides(root) {
			return nil, nil, WorldFilesPreview{}, &FileSyntaxError{
				File:    "leveldataoverride.lua",
				Line:    1,
				Message: "检测到模组配置（workshop-*），当前编辑的是世界配置 leveldataoverride.lua；请前往模组页面粘贴或确保包含 overrides 表",
			}
		}
		return nil, nil, WorldFilesPreview{}, &FileSyntaxError{
			File:    "leveldataoverride.lua",
			Line:    1,
			Message: "leveldataoverride.lua 必须包含 overrides 配置表",
		}
	}

	expectedType := canonicalWorldType(world, document)
	if actualLocation := luaLocationType(root); (expectedType == "forest" || expectedType == "cave") &&
		(actualLocation == "forest" || actualLocation == "cave") && actualLocation != expectedType {
		return nil, nil, WorldFilesPreview{}, &WorldLocationMismatchError{
			Expected: expectedType,
			Actual:   actualLocation,
		}
	}

	// Validate all override keys and JSON compatibility
	for _, entry := range overrides.entries {
		if entry.key.kind != lua.LTString || !overrideKeyPattern.MatchString(entry.key.text) {
			return nil, nil, WorldFilesPreview{}, &FieldError{
				Fields: map[string]string{"leveldataoverride.overrides": "overrides 表中存在无效的世界选项键名"},
			}
		}
		if _, convErr := nodeToJSON(entry.value); convErr != nil {
			return nil, nil, WorldFilesPreview{}, convErr
		}
	}

	patchedLua := false
	if len(request.OverridePatch) > 0 {
		if len(request.OverridePatch) > 1000 {
			return nil, nil, WorldFilesPreview{}, &FieldError{Fields: map[string]string{"overridePatch": "一次最多修改 1000 个世界选项"}}
		}
		keys := make([]string, 0, len(request.OverridePatch))
		for key := range request.OverridePatch {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !overrideKeyPattern.MatchString(key) {
				return nil, nil, WorldFilesPreview{}, &FieldError{Fields: map[string]string{"overridePatch." + key: "世界选项键格式无效"}}
			}
			raw := request.OverridePatch[key]
			if len(raw) > 64*1024 {
				return nil, nil, WorldFilesPreview{}, &FieldError{Fields: map[string]string{"overridePatch." + key: "单个世界选项不能超过 64 KiB"}}
			}
			beforeNode, existed := overrides.stringEntry(key)
			var before interface{}
			if existed {
				before, err = nodeToJSON(beforeNode)
				if err != nil {
					return nil, nil, WorldFilesPreview{}, err
				}
			}
			afterNode, err := jsonNode(raw)
			if err != nil {
				return nil, nil, WorldFilesPreview{}, &FieldError{Fields: map[string]string{"overridePatch." + key: "世界选项值不是支持的 JSON 值"}}
			}
			var after interface{}
			if afterNode != nil {
				after, err = nodeToJSON(afterNode)
				if err != nil {
					return nil, nil, WorldFilesPreview{}, err
				}
			}
			if (!existed && afterNode == nil) || (existed && afterNode != nil && reflect.DeepEqual(before, after)) {
				continue
			}
			overrides.setStringEntry(key, afterNode)
			patchedLua = true
		}
	}

	if patchedLua || (request.Format && request.Leveldataoverride != nil) || formatAll {
		luaData, err = serializeLuaReturnTable(root)
		if err != nil {
			return nil, nil, WorldFilesPreview{}, err
		}
	}

	changes, err := diffWorldDocuments(document, serverData, serverValues, luaData, root, overrides)
	if err != nil {
		return nil, nil, WorldFilesPreview{}, err
	}
	nextRevision := revision(
		revisionPart{name: "server.ini", data: serverData, exists: true},
		revisionPart{name: "leveldataoverride.lua", data: luaData, exists: true},
	)
	if len(changes) == 0 && nextRevision != document.revision {
		if !bytes.Equal(document.serverData, serverData) {
			changes = append(changes, Change{Path: "server.ini", Label: "server.ini 排版与注释", Operation: "replace"})
		}
		if !bytes.Equal(document.luaData, luaData) {
			changes = append(changes, Change{Path: "leveldataoverride.lua", Label: "leveldataoverride.lua 排版与注释", Operation: "replace"})
		}
	}
	if len(changes) == 0 && !request.AllowEmptyChanges {
		return nil, nil, WorldFilesPreview{}, ErrNoChanges
	}

	overridesMap, unknownCount, err := extractOverridesSummary(overrides)
	if err != nil {
		return nil, nil, WorldFilesPreview{}, err
	}

	return serverData, luaData, WorldFilesPreview{
		Revision:          document.revision,
		NextRevision:      nextRevision,
		Changes:           changes,
		ServerIni:         string(serverData),
		Leveldataoverride: string(luaData),
		Server:            serverValues,
		Overrides:         overridesMap,
		UnknownFieldCount: unknownCount,
		Notices:           notices,
	}, nil
}

func serverValuesFromIni(serverConfig *ini.File) WorldServerValues {
	return WorldServerValues{
		ServerPort:         serverConfig.Section("NETWORK").Key("server_port").MustInt(0),
		IsMaster:           serverConfig.Section("SHARD").Key("is_master").MustBool(false),
		ShardName:          serverConfig.Section("SHARD").Key("name").String(),
		ShardID:            serverConfig.Section("SHARD").Key("id").MustInt(-1),
		AuthenticationPort: serverConfig.Section("STEAM").Key("authentication_port").MustInt(0),
		MasterServerPort:   serverConfig.Section("STEAM").Key("master_server_port").MustInt(0),
		EncodeUserPath:     serverConfig.Section("ACCOUNT").Key("encode_user_path").MustBool(true),
	}
}

func applyServerValuesToIni(serverConfig *ini.File, before, after WorldServerValues) bool {
	changed := false
	if before.ServerPort != after.ServerPort {
		serverConfig.Section("NETWORK").Key("server_port").SetValue(strconv.Itoa(after.ServerPort))
		changed = true
	}
	if before.IsMaster != after.IsMaster {
		serverConfig.Section("SHARD").Key("is_master").SetValue(strconv.FormatBool(after.IsMaster))
		changed = true
	}
	if before.ShardName != after.ShardName {
		serverConfig.Section("SHARD").Key("name").SetValue(after.ShardName)
		changed = true
	}
	if before.ShardID != after.ShardID {
		serverConfig.Section("SHARD").Key("id").SetValue(strconv.Itoa(after.ShardID))
		changed = true
	}
	if before.AuthenticationPort != after.AuthenticationPort {
		serverConfig.Section("STEAM").Key("authentication_port").SetValue(strconv.Itoa(after.AuthenticationPort))
		changed = true
	}
	if before.MasterServerPort != after.MasterServerPort {
		serverConfig.Section("STEAM").Key("master_server_port").SetValue(strconv.Itoa(after.MasterServerPort))
		changed = true
	}
	if before.EncodeUserPath != after.EncodeUserPath {
		serverConfig.Section("ACCOUNT").Key("encode_user_path").SetValue(strconv.FormatBool(after.EncodeUserPath))
		changed = true
	}
	return changed
}

func diffWorldDocuments(beforeDoc worldDocument, afterServerData []byte, afterServer WorldServerValues, afterLuaData []byte, afterRoot, afterOverrides *luaNode) ([]Change, error) {
	_ = afterServerData
	_ = afterLuaData
	changes := append([]Change(nil), serverChanges(beforeDoc.serverValues, afterServer)...)

	// Compare root-level non-overrides keys in leveldataoverride.lua
	rootKeys := make(map[string]bool)
	for _, entry := range beforeDoc.luaRoot.entries {
		if entry.key.kind == lua.LTString && entry.key.text != "overrides" {
			rootKeys[entry.key.text] = true
		}
	}
	for _, entry := range afterRoot.entries {
		if entry.key.kind == lua.LTString && entry.key.text != "overrides" {
			rootKeys[entry.key.text] = true
		}
	}
	sortedRootKeys := make([]string, 0, len(rootKeys))
	for k := range rootKeys {
		sortedRootKeys = append(sortedRootKeys, k)
	}
	sort.Strings(sortedRootKeys)
	for _, key := range sortedRootKeys {
		beforeNode, hadBefore := beforeDoc.luaRoot.stringEntry(key)
		afterNode, hasAfter := afterRoot.stringEntry(key)
		var beforeVal, afterVal interface{}
		var err error
		if hadBefore {
			beforeVal, err = nodeToJSON(beforeNode)
			if err != nil {
				return nil, err
			}
		}
		if hasAfter {
			afterVal, err = nodeToJSON(afterNode)
			if err != nil {
				return nil, err
			}
		}
		if reflect.DeepEqual(optionalValue(hadBefore, beforeVal), optionalValue(hasAfter, afterVal)) {
			continue
		}
		changes = append(changes, Change{
			Path:      "leveldata." + key,
			Label:     key,
			Before:    optionalValue(hadBefore, beforeVal),
			After:     optionalValue(hasAfter, afterVal),
			Operation: operation(optionalValue(hadBefore, beforeVal), optionalValue(hasAfter, afterVal)),
		})
	}

	// Compare overrides keys
	overrideKeys := make(map[string]bool)
	for _, entry := range beforeDoc.overrides.entries {
		if entry.key.kind == lua.LTString {
			overrideKeys[entry.key.text] = true
		}
	}
	for _, entry := range afterOverrides.entries {
		if entry.key.kind == lua.LTString {
			overrideKeys[entry.key.text] = true
		}
	}
	sortedOverrideKeys := make([]string, 0, len(overrideKeys))
	for k := range overrideKeys {
		sortedOverrideKeys = append(sortedOverrideKeys, k)
	}
	sort.Strings(sortedOverrideKeys)
	for _, key := range sortedOverrideKeys {
		beforeNode, hadBefore := beforeDoc.overrides.stringEntry(key)
		afterNode, hasAfter := afterOverrides.stringEntry(key)
		var beforeVal, afterVal interface{}
		var err error
		if hadBefore {
			beforeVal, err = nodeToJSON(beforeNode)
			if err != nil {
				return nil, err
			}
		}
		if hasAfter {
			afterVal, err = nodeToJSON(afterNode)
			if err != nil {
				return nil, err
			}
		}
		if reflect.DeepEqual(optionalValue(hadBefore, beforeVal), optionalValue(hasAfter, afterVal)) {
			continue
		}
		changes = append(changes, Change{
			Path:      "overrides." + key,
			Label:     overrideLabel(key),
			Before:    optionalValue(hadBefore, beforeVal),
			After:     optionalValue(hasAfter, afterVal),
			Operation: operation(optionalValue(hadBefore, beforeVal), optionalValue(hasAfter, afterVal)),
		})
	}
	return changes, nil
}

func extractOverridesSummary(overrides *luaNode) (map[string]interface{}, int, error) {
	known := make(map[string]bool, len(overrideSchema))
	for _, field := range overrideSchema {
		known[field.Key] = true
	}
	result := make(map[string]interface{}, len(overrides.entries))
	unknown := 0
	for _, entry := range overrides.entries {
		if entry.key.kind != lua.LTString {
			unknown++
			continue
		}
		if !known[entry.key.text] {
			unknown++
		}
		val, err := nodeToJSON(entry.value)
		if err != nil {
			return nil, 0, err
		}
		result[entry.key.text] = val
	}
	return result, unknown, nil
}

func canonicalWorldType(world rooms.World, document worldDocument) string {
	switch string(world.Type) {
	case "forest", "cave":
		return string(world.Type)
	}
	if loc := luaLocationType(document.luaRoot); loc == "forest" || loc == "cave" {
		return loc
	}
	return "unknown"
}

func luaLocationType(root *luaNode) string {
	if root == nil {
		return ""
	}
	locNode, ok := root.stringEntry("location")
	if !ok || locNode.kind != lua.LTString {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(locNode.text))
}

func looksLikeModOverrides(root *luaNode) bool {
	if root == nil || len(root.entries) == 0 {
		return false
	}
	for _, entry := range root.entries {
		if entry.key.kind == lua.LTString && strings.HasPrefix(entry.key.text, "workshop-") {
			return true
		}
	}
	return false
}

func normalizeIniSource(raw []byte, maxBytes int64) ([]byte, error) {
	if maxBytes > 0 && int64(len(raw)) > maxBytes {
		return nil, ErrFileTooLarge
	}
	data := raw
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		data = data[3:]
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &FileSyntaxError{
			File:    "server.ini",
			Line:    1,
			Message: "server.ini 不能为空",
		}
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		copied := make([]byte, len(data)+1)
		copy(copied, data)
		copied[len(data)] = '\n'
		return copied, nil
	}
	copied := make([]byte, len(data))
	copy(copied, data)
	return copied, nil
}

func wrapLuaSyntaxError(file string, err error) error {
	var syntaxErr *luaconfig.SyntaxError
	if errors.As(err, &syntaxErr) {
		return &FileSyntaxError{
			File:    file,
			Line:    syntaxErr.Line,
			Column:  syntaxErr.Column,
			Message: syntaxErr.Message,
		}
	}
	return &FileSyntaxError{
		File:    file,
		Message: err.Error(),
	}
}
