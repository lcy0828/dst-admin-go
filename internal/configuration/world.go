package configuration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"dont/internal/roomops"
	"dont/internal/rooms"

	"github.com/go-ini/ini"
	lua "github.com/yuin/gopher-lua"
)

var overrideKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`)

var serverSchema = []FieldSchema{
	{Key: "serverPort", Group: "network", Label: "游戏端口", Type: "number", Required: true, Minimum: intPointer(1), Maximum: intPointer(65535)},
	{Key: "isMaster", Group: "shard", Label: "是否为主分片", Type: "boolean"},
	{Key: "shardName", Group: "shard", Label: "世界名称", Type: "string"},
	{Key: "shardId", Group: "shard", Label: "世界 ID", Type: "number", Required: true, Minimum: intPointer(1), Maximum: intPointer(999)},
	{Key: "authenticationPort", Group: "steam", Label: "认证端口", Type: "number", Minimum: intPointer(0), Maximum: intPointer(65535)},
	{Key: "masterServerPort", Group: "steam", Label: "主服务器端口", Type: "number", Minimum: intPointer(0), Maximum: intPointer(65535)},
	{Key: "encodeUserPath", Group: "account", Label: "编码用户路径", Type: "boolean"},
}

var frequencyOptions = []Option{
	{Value: "never", Label: "无"}, {Value: "rare", Label: "较少"}, {Value: "default", Label: "默认"},
	{Value: "often", Label: "较多"}, {Value: "always", Label: "大量"},
}

var seasonOptions = []Option{
	{Value: "noseason", Label: "无"}, {Value: "veryshortseason", Label: "极短"}, {Value: "shortseason", Label: "较短"},
	{Value: "default", Label: "默认"}, {Value: "longseason", Label: "较长"}, {Value: "verylongseason", Label: "极长"}, {Value: "random", Label: "随机"},
}

var overrideSchema = []OverrideSchema{
	{Key: "world_size", Group: "generation", Label: "世界大小", Type: "select", Options: []Option{{Value: "small", Label: "较小"}, {Value: "medium", Label: "中等"}, {Value: "default", Label: "默认"}, {Value: "huge", Label: "巨大"}}},
	{Key: "branching", Group: "generation", Label: "分支道路", Type: "select", Options: []Option{{Value: "never", Label: "无"}, {Value: "least", Label: "较少"}, {Value: "default", Label: "默认"}, {Value: "most", Label: "较多"}}},
	{Key: "loop", Group: "generation", Label: "环形道路", Type: "select", Options: []Option{{Value: "never", Label: "无"}, {Value: "default", Label: "默认"}, {Value: "always", Label: "始终"}}},
	{Key: "season_start", Group: "world", Label: "起始季节", Type: "select", Options: []Option{{Value: "autumn", Label: "秋"}, {Value: "winter", Label: "冬"}, {Value: "spring", Label: "春"}, {Value: "summer", Label: "夏"}, {Value: "default", Label: "默认"}}},
	{Key: "day", Group: "world", Label: "昼夜长度", Type: "select", Options: []Option{{Value: "onlyday", Label: "永昼"}, {Value: "longday", Label: "长昼"}, {Value: "default", Label: "默认"}, {Value: "longdusk", Label: "长黄昏"}, {Value: "longnight", Label: "长夜"}, {Value: "onlydusk", Label: "永黄昏"}, {Value: "onlynight", Label: "永夜"}}},
	{Key: "autumn", Group: "world", Label: "秋季长度", Type: "season", Options: seasonOptions},
	{Key: "winter", Group: "world", Label: "冬季长度", Type: "season", Options: seasonOptions},
	{Key: "spring", Group: "world", Label: "春季长度", Type: "season", Options: seasonOptions},
	{Key: "summer", Group: "world", Label: "夏季长度", Type: "season", Options: seasonOptions},
	{Key: "weather", Group: "world", Label: "降雨", Type: "frequency", Options: frequencyOptions},
	{Key: "lightning", Group: "world", Label: "闪电", Type: "frequency", Options: frequencyOptions},
	{Key: "wildfires", Group: "world", Label: "野火", Type: "frequency", Options: frequencyOptions},
	{Key: "hounds", Group: "creatures", Label: "猎犬袭击", Type: "frequency", Options: frequencyOptions},
	{Key: "spiders", Group: "creatures", Label: "蜘蛛", Type: "frequency", Options: frequencyOptions},
	{Key: "walrus", Group: "creatures", Label: "海象营地", Type: "frequency", Options: frequencyOptions},
	{Key: "grass", Group: "resources", Label: "草", Type: "frequency", Options: frequencyOptions},
	{Key: "sapling", Group: "resources", Label: "树苗", Type: "frequency", Options: frequencyOptions},
	{Key: "rock", Group: "resources", Label: "岩石", Type: "frequency", Options: frequencyOptions},
}

type worldDocument struct {
	serverData    []byte
	serverMode    os.FileMode
	serverValues  WorldServerValues
	luaData       []byte
	luaMode       os.FileMode
	luaRoot       *luaNode
	overrides     *luaNode
	revision      string
	modified      time.Time
	unknownFields int
	sync          SyncState
}

func (s *Service) WorldConfig(roomID, worldID string) (WorldConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.WorldConfigContext(ctx, roomID, worldID)
}

func (s *Service) WorldConfigContext(ctx context.Context, roomID, worldID string) (WorldConfig, error) {
	_, _, _, document, _, err := s.worldDocument(ctx, roomID, worldID, true)
	if err != nil {
		return WorldConfig{}, err
	}
	return worldConfigFromDocument(document)
}

func (s *Service) PreviewWorld(roomID, worldID string, request WorldUpdateRequest) (Preview, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.PreviewWorldContext(ctx, roomID, worldID, request)
}

func (s *Service) PreviewWorldContext(ctx context.Context, roomID, worldID string, request WorldUpdateRequest) (Preview, error) {
	_, _, _, document, _, err := s.worldDocument(ctx, roomID, worldID, false)
	if err != nil {
		return Preview{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return Preview{}, err
	}
	_, _, preview, err := renderWorldDocument(document, request)
	return preview, err
}

func (s *Service) ApplyWorld(ctx context.Context, jobID, roomID, worldID string, request WorldUpdateRequest) (ApplyResult, error) {
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	defer release()
	_, _, worldPath, document, routed, err := s.worldDocument(ctx, roomID, worldID, false)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return ApplyResult{}, err
	}
	_, _, preview, err := renderWorldDocument(document, request)
	if err != nil {
		return ApplyResult{}, err
	}
	_, _, _, latest, latestRouted, err := s.worldDocument(ctx, roomID, worldID, false)
	if err != nil {
		return ApplyResult{}, err
	}
	if latestRouted != routed {
		return ApplyResult{}, &RevisionConflictError{CurrentRevision: latest.revision}
	}
	if err := checkRevision(request.ExpectedRevision, latest.revision); err != nil {
		return ApplyResult{}, err
	}
	serverData, luaData, _, err := renderWorldDocument(latest, request)
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
				"server.ini": configurationFileDigest(latest.serverData), "leveldataoverride.lua": configurationFileDigest(latest.luaData),
			},
		})
		if publishErr != nil {
			return ApplyResult{}, wrapApplyError("world publication", publishErr)
		}
		sync := SyncState{Status: "synced", Source: "runtime-disk", ObservedRevision: preview.NextRevision}
		return ApplyResult{Revision: preview.NextRevision, Changes: preview.Changes, PublishedTargets: published, Sync: sync}, nil
	}
	if err := atomicWriteSet(worldPath, previous, []fileWrite{{name: "server.ini", data: serverData}, {name: "leveldataoverride.lua", data: luaData}}); err != nil {
		return ApplyResult{}, wrapApplyError("world", err)
	}
	published, err := s.publish(ctx, PublicationRequest{
		RoomID: roomID, WorldID: worldID, Scope: PublicationWorld,
		Files:   []string{"server.ini", "leveldataoverride.lua"},
		Payload: worldPublicationFiles(serverData, latest.serverMode, luaData, latest.luaMode),
	})
	if err != nil {
		rollbackErr := rollbackWrites(worldPath, previous, []string{"server.ini", "leveldataoverride.lua"})
		return ApplyResult{}, wrapApplyError("world publication", errors.Join(err, rollbackErr))
	}
	observed, err := loadWorldDocument(worldPath)
	if err != nil || observed.revision != preview.NextRevision {
		return ApplyResult{}, wrapApplyError("world verification", errors.Join(err, &RevisionConflictError{CurrentRevision: observed.revision}))
	}
	observedAt := observed.modified.UTC()
	return ApplyResult{Revision: observed.revision, Changes: preview.Changes, PublishedTargets: published, Sync: SyncState{
		Status: "synced", Source: "runtime-disk", ObservedRevision: observed.revision, ObservedAt: &observedAt,
	}}, nil
}

func (s *Service) worldDocument(ctx context.Context, roomID, worldID string, allowStale bool) (rooms.Room, rooms.World, string, worldDocument, bool, error) {
	if s.reader == nil {
		room, world, worldPath, err := s.resolveWorld(roomID, worldID)
		if err != nil {
			return rooms.Room{}, rooms.World{}, "", worldDocument{}, false, err
		}
		document, err := loadWorldDocument(worldPath)
		return room, world, worldPath, document, false, err
	}
	room, err := s.managedRoom(roomID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, "", worldDocument{}, true, err
	}
	world, err := s.rooms.World(roomID, worldID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, "", worldDocument{}, true, err
	}
	snapshot, err := s.readRuntimeConfiguration(ctx, roomID, worldID, string(PublicationWorld))
	if err != nil {
		if !allowStale {
			return rooms.Room{}, rooms.World{}, "", worldDocument{}, true, err
		}
		cached, sync, cacheErr := s.observedRuntimeConfiguration(roomID, worldID, string(PublicationWorld), err)
		if cacheErr != nil {
			return rooms.Room{}, rooms.World{}, "", worldDocument{}, true, cacheErr
		}
		snapshot = cached
		server, serverModified, fileErr := configurationSnapshotFile(snapshot.Result.Files, "server.ini", false)
		if fileErr != nil {
			return rooms.Room{}, rooms.World{}, "", worldDocument{}, true, fileErr
		}
		override, overrideModified, fileErr := configurationSnapshotFile(snapshot.Result.Files, "leveldataoverride.lua", false)
		if fileErr != nil {
			return rooms.Room{}, rooms.World{}, "", worldDocument{}, true, fileErr
		}
		document, parseErr := parseWorldDocument(server, serverModified, override, overrideModified)
		document.sync = sync
		return room, world, "", document, true, parseErr
	}
	server, serverModified, err := configurationSnapshotFile(snapshot.Result.Files, "server.ini", false)
	if err != nil {
		return rooms.Room{}, rooms.World{}, "", worldDocument{}, true, err
	}
	override, overrideModified, err := configurationSnapshotFile(snapshot.Result.Files, "leveldataoverride.lua", false)
	if err != nil {
		return rooms.Room{}, rooms.World{}, "", worldDocument{}, true, err
	}
	document, err := parseWorldDocument(server, serverModified, override, overrideModified)
	if err == nil {
		document.sync = s.observeRuntimeConfiguration(roomID, worldID, string(PublicationWorld), snapshot, document.revision, document.modified)
	}
	return room, world, "", document, true, err
}

func loadWorldDocument(worldPath string) (worldDocument, error) {
	serverData, serverMode, serverModified, serverExists, err := readConfiguration(filepath.Join(worldPath, "server.ini"), false)
	if err != nil {
		return worldDocument{}, err
	}
	luaData, luaMode, luaModified, luaExists, err := readConfiguration(filepath.Join(worldPath, "leveldataoverride.lua"), false)
	if err != nil {
		return worldDocument{}, err
	}
	return parseWorldDocument(
		fileSnapshot{data: serverData, mode: serverMode, exists: serverExists}, serverModified,
		fileSnapshot{data: luaData, mode: luaMode, exists: luaExists}, luaModified,
	)
}

func parseWorldDocument(server fileSnapshot, serverModified time.Time, override fileSnapshot, overrideModified time.Time) (worldDocument, error) {
	serverData, serverMode, serverExists := server.data, server.mode, server.exists
	luaData, luaMode, luaExists := override.data, override.mode, override.exists
	serverConfig, err := ini.Load(serverData)
	if err != nil {
		return worldDocument{}, fmt.Errorf("parse server.ini: %w", err)
	}
	root, err := parseLuaReturnTable(luaData)
	if err != nil {
		return worldDocument{}, err
	}
	overrides, ok := root.stringEntry("overrides")
	if !ok || overrides.kind != lua.LTTable {
		return worldDocument{}, errorsNew("leveldataoverride.lua must contain an overrides table")
	}
	known := make(map[string]bool, len(overrideSchema))
	for _, field := range overrideSchema {
		known[field.Key] = true
	}
	unknown := 0
	for _, entry := range overrides.entries {
		if entry.key.kind != lua.LTString || !known[entry.key.text] {
			unknown++
		}
	}
	modified := serverModified
	if overrideModified.After(modified) {
		modified = overrideModified
	}
	return worldDocument{
		serverData: serverData, serverMode: serverMode,
		serverValues: WorldServerValues{
			ServerPort:         serverConfig.Section("NETWORK").Key("server_port").MustInt(0),
			IsMaster:           serverConfig.Section("SHARD").Key("is_master").MustBool(false),
			ShardName:          serverConfig.Section("SHARD").Key("name").String(),
			ShardID:            serverConfig.Section("SHARD").Key("id").MustInt(0),
			AuthenticationPort: serverConfig.Section("STEAM").Key("authentication_port").MustInt(0),
			MasterServerPort:   serverConfig.Section("STEAM").Key("master_server_port").MustInt(0),
			EncodeUserPath:     serverConfig.Section("ACCOUNT").Key("encode_user_path").MustBool(true),
		},
		luaData: luaData, luaMode: luaMode, luaRoot: root, overrides: overrides,
		revision: revision(revisionPart{name: "server.ini", data: serverData, exists: serverExists}, revisionPart{name: "leveldataoverride.lua", data: luaData, exists: luaExists}),
		modified: modified, unknownFields: unknown,
	}, nil
}

func worldConfigFromDocument(document worldDocument) (WorldConfig, error) {
	overrides := make(map[string]interface{})
	for _, entry := range document.overrides.entries {
		if entry.key.kind != lua.LTString {
			continue
		}
		value, err := nodeToJSON(entry.value)
		if err != nil {
			return WorldConfig{}, err
		}
		overrides[entry.key.text] = value
	}
	return WorldConfig{
		Revision: document.revision, Server: document.serverValues, ServerSchema: append([]FieldSchema(nil), serverSchema...),
		Overrides: overrides, OverrideSchema: append([]OverrideSchema(nil), overrideSchema...), UnknownFieldCount: document.unknownFields, ModifiedAt: document.modified, Sync: document.sync,
	}, nil
}

func renderWorldDocument(document worldDocument, request WorldUpdateRequest) ([]byte, []byte, Preview, error) {
	if err := validateWorldServer(request.Server); err != nil {
		return nil, nil, Preview{}, err
	}
	if len(request.OverridePatch) > 1000 {
		return nil, nil, Preview{}, &FieldError{Fields: map[string]string{"overridePatch": "一次最多修改 1000 个世界选项"}}
	}
	serverConfig, err := ini.Load(document.serverData)
	if err != nil {
		return nil, nil, Preview{}, err
	}
	if document.serverValues.ServerPort != request.Server.ServerPort {
		serverConfig.Section("NETWORK").Key("server_port").SetValue(strconv.Itoa(request.Server.ServerPort))
	}
	if document.serverValues.IsMaster != request.Server.IsMaster {
		serverConfig.Section("SHARD").Key("is_master").SetValue(strconv.FormatBool(request.Server.IsMaster))
	}
	if document.serverValues.ShardName != request.Server.ShardName {
		serverConfig.Section("SHARD").Key("name").SetValue(request.Server.ShardName)
	}
	if document.serverValues.ShardID != request.Server.ShardID {
		serverConfig.Section("SHARD").Key("id").SetValue(strconv.Itoa(request.Server.ShardID))
	}
	if document.serverValues.AuthenticationPort != request.Server.AuthenticationPort {
		serverConfig.Section("STEAM").Key("authentication_port").SetValue(strconv.Itoa(request.Server.AuthenticationPort))
	}
	if document.serverValues.MasterServerPort != request.Server.MasterServerPort {
		serverConfig.Section("STEAM").Key("master_server_port").SetValue(strconv.Itoa(request.Server.MasterServerPort))
	}
	if document.serverValues.EncodeUserPath != request.Server.EncodeUserPath {
		serverConfig.Section("ACCOUNT").Key("encode_user_path").SetValue(strconv.FormatBool(request.Server.EncodeUserPath))
	}
	serverFieldChanges := serverChanges(document.serverValues, request.Server)
	serverData := document.serverData
	if len(serverFieldChanges) > 0 {
		var serverOutput bytes.Buffer
		if _, err := serverConfig.WriteTo(&serverOutput); err != nil {
			return nil, nil, Preview{}, err
		}
		serverData = serverOutput.Bytes()
	}
	root, err := parseLuaReturnTable(document.luaData)
	if err != nil {
		return nil, nil, Preview{}, err
	}
	overrides, ok := root.stringEntry("overrides")
	if !ok || overrides.kind != lua.LTTable {
		return nil, nil, Preview{}, errorsNew("leveldataoverride.lua must contain an overrides table")
	}
	changes := append([]Change(nil), serverFieldChanges...)
	overrideChangeStart := len(changes)
	keys := make([]string, 0, len(request.OverridePatch))
	for key := range request.OverridePatch {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !overrideKeyPattern.MatchString(key) {
			return nil, nil, Preview{}, &FieldError{Fields: map[string]string{"overridePatch." + key: "世界选项键格式无效"}}
		}
		raw := request.OverridePatch[key]
		if len(raw) > 64*1024 {
			return nil, nil, Preview{}, &FieldError{Fields: map[string]string{"overridePatch." + key: "单个世界选项不能超过 64 KiB"}}
		}
		beforeNode, existed := overrides.stringEntry(key)
		var before interface{}
		if existed {
			before, err = nodeToJSON(beforeNode)
			if err != nil {
				return nil, nil, Preview{}, err
			}
		}
		afterNode, err := jsonNode(raw)
		if err != nil {
			return nil, nil, Preview{}, &FieldError{Fields: map[string]string{"overridePatch." + key: "世界选项值不是支持的 JSON 值"}}
		}
		var after interface{}
		if afterNode != nil {
			after, err = nodeToJSON(afterNode)
			if err != nil {
				return nil, nil, Preview{}, err
			}
		}
		if (!existed && afterNode == nil) || (existed && afterNode != nil && reflect.DeepEqual(before, after)) {
			continue
		}
		overrides.setStringEntry(key, afterNode)
		changes = append(changes, Change{Path: "overrides." + key, Label: overrideLabel(key), Before: before, After: after, Operation: operation(optionalValue(existed, before), after)})
	}
	if len(changes) == 0 {
		return nil, nil, Preview{}, ErrNoChanges
	}
	luaOutput := document.luaData
	if len(changes) > overrideChangeStart {
		luaOutput, err = serializeLuaReturnTable(root)
		if err != nil {
			return nil, nil, Preview{}, err
		}
	}
	nextRevision := revision(
		revisionPart{name: "server.ini", data: serverData, exists: true},
		revisionPart{name: "leveldataoverride.lua", data: luaOutput, exists: true},
	)
	return serverData, luaOutput, Preview{Revision: document.revision, NextRevision: nextRevision, Changes: changes}, nil
}

func validateWorldServer(values WorldServerValues) error {
	fields := make(map[string]string)
	if values.ServerPort < 1 || values.ServerPort > 65535 {
		fields["server.serverPort"] = "游戏端口必须在 1-65535 之间"
	}
	if len([]rune(values.ShardName)) > 64 || strings.ContainsAny(values.ShardName, "\x00\r\n") {
		fields["server.shardName"] = "世界名称不能超过 64 个字符且不能包含换行"
	}
	if values.ShardID < 1 || values.ShardID > 999 {
		fields["server.shardId"] = "世界 ID 必须在 1-999 之间"
	}
	for key, value := range map[string]int{"server.authenticationPort": values.AuthenticationPort, "server.masterServerPort": values.MasterServerPort} {
		if value < 0 || value > 65535 {
			fields[key] = "端口必须为 0 或 1-65535"
		}
	}
	ports := []int{values.ServerPort, values.AuthenticationPort, values.MasterServerPort}
	for index, left := range ports {
		if left == 0 {
			continue
		}
		for _, right := range ports[index+1:] {
			if left == right {
				fields["server"] = "同一世界的端口不能重复"
			}
		}
	}
	if len(fields) > 0 {
		return &FieldError{Fields: fields}
	}
	return nil
}

func serverChanges(before, after WorldServerValues) []Change {
	result := make([]Change, 0, 7)
	items := []struct {
		key           string
		before, after interface{}
	}{
		{"serverPort", before.ServerPort, after.ServerPort}, {"isMaster", before.IsMaster, after.IsMaster},
		{"shardName", before.ShardName, after.ShardName}, {"shardId", before.ShardID, after.ShardID},
		{"authenticationPort", before.AuthenticationPort, after.AuthenticationPort},
		{"masterServerPort", before.MasterServerPort, after.MasterServerPort}, {"encodeUserPath", before.EncodeUserPath, after.EncodeUserPath},
	}
	for _, item := range items {
		if reflect.DeepEqual(item.before, item.after) {
			continue
		}
		result = append(result, Change{Path: "server." + item.key, Label: fieldLabel(serverSchema, item.key), Before: item.before, After: item.after, Operation: "replace"})
	}
	return result
}

func overrideLabel(key string) string {
	for _, field := range overrideSchema {
		if field.Key == key {
			return field.Label
		}
	}
	return key
}

func optionalValue(exists bool, value interface{}) interface{} {
	if !exists {
		return nil
	}
	return value
}

func errorsNew(message string) error { return fmt.Errorf("%w: %s", ErrInvalidConfiguration, message) }
