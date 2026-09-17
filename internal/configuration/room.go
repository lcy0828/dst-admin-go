package configuration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"dont/internal/roomops"
	"dont/internal/rooms"

	"github.com/go-ini/ini"
)

var roomSchema = []FieldSchema{
	{Key: "clusterName", Group: "network", Label: "房间名称", Type: "string", Required: true},
	{Key: "clusterDescription", Group: "network", Label: "房间描述", Type: "textarea"},
	{Key: "clusterPassword", Group: "network", Label: "房间密码", Type: "password", Secret: true},
	{Key: "clusterIntention", Group: "network", Label: "游戏偏好", Type: "select", Options: []Option{{Value: "cooperative", Label: "合作"}, {Value: "competitive", Label: "竞争"}, {Value: "social", Label: "社交"}, {Value: "madness", Label: "疯狂"}}},
	{Key: "clusterLanguage", Group: "network", Label: "服务器语言", Type: "select", Options: []Option{{Value: "zh", Label: "中文"}, {Value: "en", Label: "英文"}}},
	{Key: "gameMode", Group: "gameplay", Label: "游戏模式", Type: "select", Required: true, Options: []Option{{Value: "survival", Label: "生存"}, {Value: "endless", Label: "无尽"}, {Value: "wilderness", Label: "荒野"}}},
	{Key: "maxPlayers", Group: "gameplay", Label: "玩家上限", Type: "number", Required: true, Minimum: intPointer(1), Maximum: intPointer(64)},
	{Key: "pvp", Group: "gameplay", Label: "玩家对战", Type: "boolean"},
	{Key: "pauseWhenEmpty", Group: "gameplay", Label: "无人时暂停", Type: "boolean"},
	{Key: "voteEnabled", Group: "gameplay", Label: "启用投票", Type: "boolean"},
	{Key: "voteKickEnabled", Group: "gameplay", Label: "投票踢人", Type: "boolean"},
	{Key: "consoleEnabled", Group: "system", Label: "启用控制台", Type: "boolean"},
	{Key: "lanOnly", Group: "network", Label: "仅局域网", Type: "boolean"},
	{Key: "offline", Group: "network", Label: "离线模式", Type: "boolean"},
	{Key: "whitelistSlots", Group: "network", Label: "白名单预留位", Type: "number", Minimum: intPointer(0), Maximum: intPointer(64)},
	{Key: "tickRate", Group: "network", Label: "通信频率", Type: "number", Minimum: intPointer(15), Maximum: intPointer(60)},
	{Key: "autosaverEnabled", Group: "network", Label: "自动保存", Type: "boolean"},
	{Key: "idleTimeout", Group: "network", Label: "挂机超时", Type: "number", Minimum: intPointer(0)},
	{Key: "maxSnapshots", Group: "system", Label: "最大快照数", Type: "number", Minimum: intPointer(1)},
	{Key: "shardEnabled", Group: "shard", Label: "开启服务器共享", Type: "boolean"},
	{Key: "bindIp", Group: "shard", Label: "监听地址", Type: "string"},
	{Key: "masterIp", Group: "shard", Label: "主服务器 IP", Type: "string"},
	{Key: "masterPort", Group: "shard", Label: "主服务器端口", Type: "number", Minimum: intPointer(1), Maximum: intPointer(65535)},
	{Key: "clusterKey", Group: "shard", Label: "分片连接密码", Type: "password", Secret: true},
	{Key: "steamGroupOnly", Group: "steam", Label: "仅 Steam 组", Type: "boolean"},
	{Key: "steamGroupId", Group: "steam", Label: "Steam 组 ID", Type: "number", Minimum: intPointer(0)},
	{Key: "steamGroupAdmins", Group: "steam", Label: "组管理员权限", Type: "boolean"},
}

var roomKnownKeys = map[string]bool{
	"GAMEPLAY\x00game_mode": true, "GAMEPLAY\x00max_players": true, "GAMEPLAY\x00pvp": true,
	"GAMEPLAY\x00pause_when_empty": true, "GAMEPLAY\x00vote_enabled": true,
	"GAMEPLAY\x00vote_kick_enabled": true,
	"NETWORK\x00cluster_name":       true, "NETWORK\x00cluster_description": true, "NETWORK\x00cluster_password": true,
	"NETWORK\x00cluster_intention": true, "NETWORK\x00cluster_language": true,
	"NETWORK\x00lan_only_cluster": true, "NETWORK\x00offline_cluster": true, "NETWORK\x00whitelist_slots": true,
	"NETWORK\x00tick_rate": true, "NETWORK\x00autosaver_enabled": true, "NETWORK\x00idle_timeout": true,
	"MISC\x00console_enabled": true, "MISC\x00max_snapshots": true,
	"SHARD\x00shard_enabled": true, "SHARD\x00bind_ip": true, "SHARD\x00master_ip": true,
	"SHARD\x00master_port": true, "SHARD\x00cluster_key": true,
	"STEAM\x00steam_group_only": true, "STEAM\x00steam_group_id": true, "STEAM\x00steam_group_admins": true,
}

type roomDocument struct {
	config                    *ini.File
	data                      []byte
	mode                      os.FileMode
	modified                  time.Time
	revision                  string
	values                    RoomValues
	unknown                   int
	clusterLanguageConfigured bool
	sync                      SyncState
}

func (s *Service) RoomConfig(roomID string) (RoomConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.RoomConfigContext(ctx, roomID)
}

func (s *Service) RoomConfigContext(ctx context.Context, roomID string) (RoomConfig, error) {
	_, _, document, _, err := s.roomDocument(ctx, roomID, true)
	if err != nil {
		return RoomConfig{}, err
	}
	return roomConfigFromDocument(document), nil
}

func (s *Service) PreviewRoom(roomID string, request RoomUpdateRequest) (Preview, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.PreviewRoomContext(ctx, roomID, request)
}

func (s *Service) PreviewRoomContext(ctx context.Context, roomID string, request RoomUpdateRequest) (Preview, error) {
	_, _, document, _, err := s.roomDocument(ctx, roomID, false)
	if err != nil {
		return Preview{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return Preview{}, err
	}
	return prepareRoomUpdate(document, request.Values)
}

func (s *Service) ApplyRoom(ctx context.Context, jobID, roomID string, request RoomUpdateRequest) (ApplyResult, error) {
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	defer release()
	_, roomPath, document, routed, err := s.roomDocument(ctx, roomID, false)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return ApplyResult{}, err
	}
	preview, err := prepareRoomUpdate(document, request.Values)
	if err != nil {
		return ApplyResult{}, err
	}
	_, _, latest, latestRouted, err := s.roomDocument(ctx, roomID, false)
	if err != nil {
		return ApplyResult{}, err
	}
	if latestRouted != routed {
		return ApplyResult{}, &RevisionConflictError{CurrentRevision: latest.revision}
	}
	if err := checkRevision(request.ExpectedRevision, latest.revision); err != nil {
		return ApplyResult{}, err
	}
	next, _, err := renderRoomDocument(latest, request.Values)
	if err != nil {
		return ApplyResult{}, err
	}
	if routed {
		if s.publisher == nil {
			return ApplyResult{}, wrapApplyError("room publication", errors.New("configuration publisher is unavailable"))
		}
		publicationFiles := roomPublicationFiles(next, latest.mode)
		published, publishErr := s.publish(ctx, PublicationRequest{
			RoomID: roomID, Scope: PublicationShared, Files: []string{"cluster.ini"}, IncludeLocal: true,
			Payload: publicationFiles, ExpectedFiles: map[string]string{"cluster.ini": configurationFileDigest(latest.data)},
		})
		if publishErr != nil {
			return ApplyResult{}, wrapApplyError("room publication", publishErr)
		}
		sync := SyncState{Status: "synced", Source: "runtime-disk", ObservedRevision: preview.NextRevision}
		return ApplyResult{Revision: preview.NextRevision, Changes: preview.Changes, PublishedTargets: published, Sync: sync}, nil
	}
	if err := atomicWrite(filepath.Join(roomPath, "cluster.ini"), next, latest.mode); err != nil {
		return ApplyResult{}, wrapApplyError("room", err)
	}
	published, err := s.publish(ctx, PublicationRequest{
		RoomID: roomID, Scope: PublicationShared, Files: []string{"cluster.ini"},
		Payload: roomPublicationFiles(next, latest.mode),
	})
	if err != nil {
		rollbackErr := atomicWrite(filepath.Join(roomPath, "cluster.ini"), latest.data, latest.mode)
		return ApplyResult{}, wrapApplyError("room publication", errors.Join(err, rollbackErr))
	}
	observed, err := loadRoomDocument(roomPath)
	if err != nil || observed.revision != preview.NextRevision {
		return ApplyResult{}, wrapApplyError("room verification", errors.Join(err, &RevisionConflictError{CurrentRevision: observed.revision}))
	}
	observedAt := observed.modified.UTC()
	return ApplyResult{Revision: observed.revision, Changes: preview.Changes, PublishedTargets: published, Sync: SyncState{
		Status: "synced", Source: "runtime-disk", ObservedRevision: observed.revision, ObservedAt: &observedAt,
	}}, nil
}

func (s *Service) roomDocument(ctx context.Context, roomID string, allowStale bool) (rooms.Room, string, roomDocument, bool, error) {
	if s.reader == nil {
		room, roomPath, err := s.resolveRoom(roomID)
		if err != nil {
			return rooms.Room{}, "", roomDocument{}, false, err
		}
		document, err := loadRoomDocument(roomPath)
		return room, roomPath, document, false, err
	}
	room, err := s.managedRoom(roomID)
	if err != nil {
		return rooms.Room{}, "", roomDocument{}, true, err
	}
	snapshot, err := s.readRuntimeConfiguration(ctx, roomID, "", string(PublicationShared))
	if err != nil {
		if !allowStale {
			return rooms.Room{}, "", roomDocument{}, true, err
		}
		cached, sync, cacheErr := s.observedRuntimeConfiguration(roomID, "", string(PublicationShared), err)
		if cacheErr != nil {
			return rooms.Room{}, "", roomDocument{}, true, cacheErr
		}
		snapshot = cached
		file, modified, fileErr := configurationSnapshotFile(snapshot.Result.Files, "cluster.ini", false)
		if fileErr != nil {
			return rooms.Room{}, "", roomDocument{}, true, fileErr
		}
		document, parseErr := parseRoomDocument(file.data, file.mode, modified, file.exists)
		document.sync = sync
		return room, "", document, true, parseErr
	}
	file, modified, err := configurationSnapshotFile(snapshot.Result.Files, "cluster.ini", false)
	if err != nil {
		return rooms.Room{}, "", roomDocument{}, true, err
	}
	document, err := parseRoomDocument(file.data, file.mode, modified, file.exists)
	if err == nil {
		document.sync = s.observeRuntimeConfiguration(roomID, "", string(PublicationShared), snapshot, document.revision, modified)
	}
	return room, "", document, true, err
}

func loadRoomDocument(roomPath string) (roomDocument, error) {
	path := filepath.Join(roomPath, "cluster.ini")
	data, mode, modified, exists, err := readConfiguration(path, false)
	if err != nil {
		return roomDocument{}, err
	}
	return parseRoomDocument(data, mode, modified, exists)
}

func parseRoomDocument(data []byte, mode os.FileMode, modified time.Time, exists bool) (roomDocument, error) {
	config, err := ini.Load(data)
	if err != nil {
		return roomDocument{}, fmt.Errorf("parse cluster.ini: %w", err)
	}
	clusterLanguageConfigured := config.Section("NETWORK").HasKey("cluster_language")
	values := RoomValues{
		ClusterName:        strings.TrimSpace(config.Section("NETWORK").Key("cluster_name").String()),
		ClusterDescription: config.Section("NETWORK").Key("cluster_description").String(),
		ClusterPassword:    config.Section("NETWORK").Key("cluster_password").String(),
		ClusterIntention:   config.Section("NETWORK").Key("cluster_intention").MustString("cooperative"),
		ClusterLanguage:    config.Section("NETWORK").Key("cluster_language").MustString("zh"),
		GameMode:           config.Section("GAMEPLAY").Key("game_mode").MustString("survival"),
		MaxPlayers:         config.Section("GAMEPLAY").Key("max_players").MustInt(6),
		PvP:                config.Section("GAMEPLAY").Key("pvp").MustBool(false),
		PauseWhenEmpty:     config.Section("GAMEPLAY").Key("pause_when_empty").MustBool(true),
		VoteEnabled:        config.Section("GAMEPLAY").Key("vote_enabled").MustBool(true),
		VoteKickEnabled:    config.Section("GAMEPLAY").Key("vote_kick_enabled").MustBool(false),
		ConsoleEnabled:     config.Section("MISC").Key("console_enabled").MustBool(true),
		LANOnly:            config.Section("NETWORK").Key("lan_only_cluster").MustBool(false),
		Offline:            config.Section("NETWORK").Key("offline_cluster").MustBool(false),
		WhitelistSlots:     config.Section("NETWORK").Key("whitelist_slots").MustInt(0),
		TickRate:           config.Section("NETWORK").Key("tick_rate").MustInt(15),
		AutosaverEnabled:   config.Section("NETWORK").Key("autosaver_enabled").MustBool(true),
		IdleTimeout:        config.Section("NETWORK").Key("idle_timeout").MustInt(0),
		MaxSnapshots:       config.Section("MISC").Key("max_snapshots").MustInt(10),
		ShardEnabled:       config.Section("SHARD").Key("shard_enabled").MustBool(true),
		BindIP:             config.Section("SHARD").Key("bind_ip").MustString("127.0.0.1"),
		MasterIP:           config.Section("SHARD").Key("master_ip").MustString("127.0.0.1"),
		MasterPort:         config.Section("SHARD").Key("master_port").MustInt(10889),
		ClusterKey:         config.Section("SHARD").Key("cluster_key").String(),
		SteamGroupOnly:     config.Section("STEAM").Key("steam_group_only").MustBool(false),
		SteamGroupID:       config.Section("STEAM").Key("steam_group_id").MustInt64(0),
		SteamGroupAdmins:   config.Section("STEAM").Key("steam_group_admins").MustBool(false),
	}
	unknown := 0
	for _, section := range config.Sections() {
		for _, key := range section.Keys() {
			if !roomKnownKeys[section.Name()+"\x00"+key.Name()] {
				unknown++
			}
		}
	}
	return roomDocument{
		config:                    config,
		data:                      data,
		mode:                      mode,
		modified:                  modified,
		revision:                  revision(revisionPart{name: "cluster.ini", data: data, exists: exists}),
		values:                    values,
		unknown:                   unknown,
		clusterLanguageConfigured: clusterLanguageConfigured,
	}, nil
}

func roomConfigFromDocument(document roomDocument) RoomConfig {
	return RoomConfig{Revision: document.revision, Values: document.values, Schema: append([]FieldSchema(nil), roomSchema...), UnknownFieldCount: document.unknown, ModifiedAt: document.modified, Sync: document.sync}
}

func prepareRoomUpdate(document roomDocument, values RoomValues) (Preview, error) {
	next, changes, err := renderRoomDocument(document, values)
	if err != nil {
		return Preview{}, err
	}
	if len(changes) == 0 {
		return Preview{}, ErrNoChanges
	}
	return Preview{Revision: document.revision, NextRevision: revision(revisionPart{name: "cluster.ini", data: next, exists: true}), Changes: changes}, nil
}

func renderRoomDocument(document roomDocument, values RoomValues) ([]byte, []Change, error) {
	values.ClusterName = strings.TrimSpace(values.ClusterName)
	if err := validateRoomValues(values); err != nil {
		return nil, nil, err
	}
	config, err := ini.Load(document.data)
	if err != nil {
		return nil, nil, err
	}
	setRoomValues(config, document.values, values)
	if !document.clusterLanguageConfigured {
		config.Section("NETWORK").Key("cluster_language").SetValue(values.ClusterLanguage)
	}
	var output bytes.Buffer
	if _, err := config.WriteTo(&output); err != nil {
		return nil, nil, err
	}
	changes := roomChanges(document, values)
	return output.Bytes(), changes, nil
}

func validateRoomValues(values RoomValues) error {
	fields := make(map[string]string)
	values.ClusterName = strings.TrimSpace(values.ClusterName)
	if values.ClusterName == "" || len([]rune(values.ClusterName)) > 64 || strings.ContainsAny(values.ClusterName, "\x00\r\n") {
		fields["clusterName"] = "房间名称必须为 1-64 个字符且不能包含换行"
	}
	if len([]rune(values.ClusterDescription)) > 512 || strings.ContainsAny(values.ClusterDescription, "\x00\r\n") {
		fields["clusterDescription"] = "房间描述不能超过 512 个字符且不能包含换行"
	}
	if len([]rune(values.ClusterPassword)) > 64 || strings.ContainsAny(values.ClusterPassword, "\x00\r\n") {
		fields["clusterPassword"] = "房间密码不能超过 64 个字符且不能包含换行"
	}
	if values.GameMode != "survival" && values.GameMode != "endless" && values.GameMode != "wilderness" {
		fields["gameMode"] = "游戏模式必须为 survival、endless 或 wilderness"
	}
	if values.MaxPlayers < 1 || values.MaxPlayers > 64 {
		fields["maxPlayers"] = "玩家上限必须在 1-64 之间"
	}
	if values.ClusterIntention != "cooperative" && values.ClusterIntention != "competitive" && values.ClusterIntention != "social" && values.ClusterIntention != "madness" {
		fields["clusterIntention"] = "游戏偏好必须为 cooperative、competitive、social 或 madness"
	}
	if values.ClusterLanguage != "zh" && values.ClusterLanguage != "en" {
		fields["clusterLanguage"] = "服务器语言必须为 zh 或 en"
	}
	if values.WhitelistSlots < 0 || values.WhitelistSlots > values.MaxPlayers {
		fields["whitelistSlots"] = "白名单预留位必须在 0 到玩家上限之间"
	}
	if values.TickRate < 15 || values.TickRate > 60 {
		fields["tickRate"] = "通信频率必须在 15-60 之间"
	}
	if values.IdleTimeout < 0 {
		fields["idleTimeout"] = "挂机超时不能小于 0"
	}
	if values.MaxSnapshots < 1 {
		fields["maxSnapshots"] = "最大快照数不能小于 1"
	}
	if strings.ContainsAny(values.BindIP, "\x00\r\n") || len(values.BindIP) > 255 {
		fields["bindIp"] = "监听地址格式无效"
	}
	if strings.ContainsAny(values.MasterIP, "\x00\r\n") || len(values.MasterIP) > 255 {
		fields["masterIp"] = "主服务器 IP 格式无效"
	}
	if ip := net.ParseIP(strings.TrimSpace(values.MasterIP)); values.ShardEnabled && ip != nil && ip.IsUnspecified() {
		fields["masterIp"] = "主服务器连接地址不能使用 0.0.0.0 或 ::；同机部署请使用 127.0.0.1，跨机器部署请填写可达地址"
	}
	if values.MasterPort < 1 || values.MasterPort > 65535 {
		fields["masterPort"] = "主服务器端口必须在 1-65535 之间"
	}
	if strings.ContainsAny(values.ClusterKey, "\x00\r\n") || len(values.ClusterKey) > 256 {
		fields["clusterKey"] = "分片连接密码格式无效"
	}
	if values.SteamGroupID < 0 {
		fields["steamGroupId"] = "Steam 组 ID 不能小于 0"
	}
	if len(fields) > 0 {
		return &FieldError{Fields: fields}
	}
	return nil
}

func setRoomValues(config *ini.File, before, after RoomValues) {
	entries := []struct {
		section, key, value string
		changed             bool
	}{
		{"NETWORK", "cluster_name", strings.TrimSpace(after.ClusterName), before.ClusterName != strings.TrimSpace(after.ClusterName)},
		{"NETWORK", "cluster_description", after.ClusterDescription, before.ClusterDescription != after.ClusterDescription},
		{"NETWORK", "cluster_password", after.ClusterPassword, before.ClusterPassword != after.ClusterPassword},
		{"NETWORK", "cluster_intention", after.ClusterIntention, before.ClusterIntention != after.ClusterIntention},
		{"NETWORK", "cluster_language", after.ClusterLanguage, before.ClusterLanguage != after.ClusterLanguage},
		{"NETWORK", "lan_only_cluster", strconv.FormatBool(after.LANOnly), before.LANOnly != after.LANOnly},
		{"NETWORK", "offline_cluster", strconv.FormatBool(after.Offline), before.Offline != after.Offline},
		{"NETWORK", "whitelist_slots", strconv.Itoa(after.WhitelistSlots), before.WhitelistSlots != after.WhitelistSlots},
		{"NETWORK", "tick_rate", strconv.Itoa(after.TickRate), before.TickRate != after.TickRate},
		{"NETWORK", "autosaver_enabled", strconv.FormatBool(after.AutosaverEnabled), before.AutosaverEnabled != after.AutosaverEnabled},
		{"NETWORK", "idle_timeout", strconv.Itoa(after.IdleTimeout), before.IdleTimeout != after.IdleTimeout},
		{"GAMEPLAY", "game_mode", after.GameMode, before.GameMode != after.GameMode},
		{"GAMEPLAY", "max_players", strconv.Itoa(after.MaxPlayers), before.MaxPlayers != after.MaxPlayers},
		{"GAMEPLAY", "pvp", strconv.FormatBool(after.PvP), before.PvP != after.PvP},
		{"GAMEPLAY", "pause_when_empty", strconv.FormatBool(after.PauseWhenEmpty), before.PauseWhenEmpty != after.PauseWhenEmpty},
		{"GAMEPLAY", "vote_enabled", strconv.FormatBool(after.VoteEnabled), before.VoteEnabled != after.VoteEnabled},
		{"GAMEPLAY", "vote_kick_enabled", strconv.FormatBool(after.VoteKickEnabled), before.VoteKickEnabled != after.VoteKickEnabled},
		{"MISC", "console_enabled", strconv.FormatBool(after.ConsoleEnabled), before.ConsoleEnabled != after.ConsoleEnabled},
		{"MISC", "max_snapshots", strconv.Itoa(after.MaxSnapshots), before.MaxSnapshots != after.MaxSnapshots},
		{"SHARD", "shard_enabled", strconv.FormatBool(after.ShardEnabled), before.ShardEnabled != after.ShardEnabled},
		{"SHARD", "bind_ip", after.BindIP, before.BindIP != after.BindIP},
		{"SHARD", "master_ip", after.MasterIP, before.MasterIP != after.MasterIP},
		{"SHARD", "master_port", strconv.Itoa(after.MasterPort), before.MasterPort != after.MasterPort},
		{"SHARD", "cluster_key", after.ClusterKey, before.ClusterKey != after.ClusterKey},
		{"STEAM", "steam_group_only", strconv.FormatBool(after.SteamGroupOnly), before.SteamGroupOnly != after.SteamGroupOnly},
		{"STEAM", "steam_group_id", strconv.FormatInt(after.SteamGroupID, 10), before.SteamGroupID != after.SteamGroupID},
		{"STEAM", "steam_group_admins", strconv.FormatBool(after.SteamGroupAdmins), before.SteamGroupAdmins != after.SteamGroupAdmins},
	}
	for _, entry := range entries {
		if !entry.changed {
			continue
		}
		config.Section(entry.section).Key(entry.key).SetValue(entry.value)
	}
}

func roomChanges(document roomDocument, after RoomValues) []Change {
	before := document.values
	valuesBefore := map[string]interface{}{
		"clusterName": before.ClusterName, "clusterDescription": before.ClusterDescription, "clusterPassword": before.ClusterPassword,
		"clusterIntention": before.ClusterIntention, "clusterLanguage": before.ClusterLanguage,
		"gameMode": before.GameMode, "maxPlayers": before.MaxPlayers, "pvp": before.PvP, "pauseWhenEmpty": before.PauseWhenEmpty,
		"voteEnabled": before.VoteEnabled, "voteKickEnabled": before.VoteKickEnabled, "consoleEnabled": before.ConsoleEnabled,
		"lanOnly": before.LANOnly, "offline": before.Offline, "whitelistSlots": before.WhitelistSlots, "tickRate": before.TickRate,
		"autosaverEnabled": before.AutosaverEnabled, "idleTimeout": before.IdleTimeout, "maxSnapshots": before.MaxSnapshots,
		"shardEnabled": before.ShardEnabled, "bindIp": before.BindIP, "masterIp": before.MasterIP, "masterPort": before.MasterPort,
		"clusterKey": before.ClusterKey, "steamGroupOnly": before.SteamGroupOnly, "steamGroupId": before.SteamGroupID,
		"steamGroupAdmins": before.SteamGroupAdmins,
	}
	valuesAfter := map[string]interface{}{
		"clusterName": after.ClusterName, "clusterDescription": after.ClusterDescription, "clusterPassword": after.ClusterPassword,
		"clusterIntention": after.ClusterIntention, "clusterLanguage": after.ClusterLanguage,
		"gameMode": after.GameMode, "maxPlayers": after.MaxPlayers, "pvp": after.PvP, "pauseWhenEmpty": after.PauseWhenEmpty,
		"voteEnabled": after.VoteEnabled, "voteKickEnabled": after.VoteKickEnabled, "consoleEnabled": after.ConsoleEnabled,
		"lanOnly": after.LANOnly, "offline": after.Offline, "whitelistSlots": after.WhitelistSlots, "tickRate": after.TickRate,
		"autosaverEnabled": after.AutosaverEnabled, "idleTimeout": after.IdleTimeout, "maxSnapshots": after.MaxSnapshots,
		"shardEnabled": after.ShardEnabled, "bindIp": after.BindIP, "masterIp": after.MasterIP, "masterPort": after.MasterPort,
		"clusterKey": after.ClusterKey, "steamGroupOnly": after.SteamGroupOnly, "steamGroupId": after.SteamGroupID,
		"steamGroupAdmins": after.SteamGroupAdmins,
	}
	keys := make([]string, 0, len(valuesBefore))
	for key := range valuesBefore {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changes := make([]Change, 0)
	for _, key := range keys {
		isMissingLanguage := key == "clusterLanguage" && !document.clusterLanguageConfigured
		if reflect.DeepEqual(valuesBefore[key], valuesAfter[key]) && !isMissingLanguage {
			continue
		}
		change := Change{Path: "cluster." + key, Label: fieldLabel(roomSchema, key), Before: valuesBefore[key], After: valuesAfter[key], Operation: "replace"}
		if isMissingLanguage {
			change.Before = nil
			change.Operation = "add"
		}
		if key == "clusterPassword" || key == "clusterKey" {
			change.Sensitive = true
			if key == "clusterPassword" {
				change.Before = configuredLabel(before.ClusterPassword)
				change.After = configuredLabel(after.ClusterPassword)
			} else {
				change.Before = configuredLabel(before.ClusterKey)
				change.After = configuredLabel(after.ClusterKey)
			}
		}
		changes = append(changes, change)
	}
	return changes
}

func configuredLabel(value string) string {
	if value == "" {
		return "未设置"
	}
	return "已设置"
}
