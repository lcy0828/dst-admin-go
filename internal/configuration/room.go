package configuration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-ini/ini"
)

var roomSchema = []FieldSchema{
	{Key: "clusterName", Group: "network", Label: "房间名称", Type: "string", Required: true},
	{Key: "clusterDescription", Group: "network", Label: "房间描述", Type: "textarea"},
	{Key: "clusterPassword", Group: "network", Label: "房间密码", Type: "password", Secret: true},
	{Key: "gameMode", Group: "gameplay", Label: "游戏模式", Type: "select", Required: true, Options: []Option{{Value: "survival", Label: "生存"}, {Value: "endless", Label: "无尽"}, {Value: "wilderness", Label: "荒野"}}},
	{Key: "maxPlayers", Group: "gameplay", Label: "玩家上限", Type: "number", Required: true, Minimum: intPointer(1), Maximum: intPointer(64)},
	{Key: "pvp", Group: "gameplay", Label: "玩家对战", Type: "boolean"},
	{Key: "pauseWhenEmpty", Group: "gameplay", Label: "无人时暂停", Type: "boolean"},
	{Key: "voteEnabled", Group: "gameplay", Label: "启用投票", Type: "boolean"},
	{Key: "consoleEnabled", Group: "system", Label: "启用控制台", Type: "boolean"},
	{Key: "lanOnly", Group: "network", Label: "仅局域网", Type: "boolean"},
	{Key: "offline", Group: "network", Label: "离线模式", Type: "boolean"},
}

var roomKnownKeys = map[string]bool{
	"GAMEPLAY\x00game_mode": true, "GAMEPLAY\x00max_players": true, "GAMEPLAY\x00pvp": true,
	"GAMEPLAY\x00pause_when_empty": true, "GAMEPLAY\x00vote_enabled": true,
	"NETWORK\x00cluster_name": true, "NETWORK\x00cluster_description": true, "NETWORK\x00cluster_password": true,
	"NETWORK\x00lan_only_cluster": true, "NETWORK\x00offline_cluster": true,
	"MISC\x00console_enabled": true,
}

type roomDocument struct {
	config   *ini.File
	data     []byte
	mode     os.FileMode
	modified time.Time
	revision string
	values   RoomValues
	unknown  int
}

func (s *Service) RoomConfig(roomID string) (RoomConfig, error) {
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	_, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return RoomConfig{}, err
	}
	document, err := loadRoomDocument(roomPath)
	if err != nil {
		return RoomConfig{}, err
	}
	return roomConfigFromDocument(document), nil
}

func (s *Service) PreviewRoom(roomID string, request RoomUpdateRequest) (Preview, error) {
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	_, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return Preview{}, err
	}
	document, err := loadRoomDocument(roomPath)
	if err != nil {
		return Preview{}, err
	}
	if err := checkRevision(request.ExpectedRevision, document.revision); err != nil {
		return Preview{}, err
	}
	return prepareRoomUpdate(document, request.Values)
}

func (s *Service) ApplyRoom(ctx context.Context, jobID, roomID string, request RoomUpdateRequest) (ApplyResult, error) {
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ApplyResult{}, err
	}
	document, err := loadRoomDocument(roomPath)
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
	backup, err := s.protectionBackup(ctx, room, "房间配置", jobID)
	if err != nil {
		return ApplyResult{}, wrapApplyError("room", err)
	}
	latest, err := loadRoomDocument(roomPath)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := checkRevision(request.ExpectedRevision, latest.revision); err != nil {
		return ApplyResult{}, err
	}
	next, _, err := renderRoomDocument(latest, request.Values)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := atomicWrite(filepath.Join(roomPath, "cluster.ini"), next, latest.mode); err != nil {
		return ApplyResult{}, wrapApplyError("room", err)
	}
	return ApplyResult{Revision: preview.NextRevision, Changes: preview.Changes, ProtectionBackupID: backup.ID}, nil
}

func loadRoomDocument(roomPath string) (roomDocument, error) {
	path := filepath.Join(roomPath, "cluster.ini")
	data, mode, modified, exists, err := readConfiguration(path, false)
	if err != nil {
		return roomDocument{}, err
	}
	config, err := ini.Load(data)
	if err != nil {
		return roomDocument{}, fmt.Errorf("parse cluster.ini: %w", err)
	}
	values := RoomValues{
		ClusterName:        strings.TrimSpace(config.Section("NETWORK").Key("cluster_name").String()),
		ClusterDescription: config.Section("NETWORK").Key("cluster_description").String(),
		ClusterPassword:    config.Section("NETWORK").Key("cluster_password").String(),
		GameMode:           config.Section("GAMEPLAY").Key("game_mode").MustString("survival"),
		MaxPlayers:         config.Section("GAMEPLAY").Key("max_players").MustInt(6),
		PvP:                config.Section("GAMEPLAY").Key("pvp").MustBool(false),
		PauseWhenEmpty:     config.Section("GAMEPLAY").Key("pause_when_empty").MustBool(true),
		VoteEnabled:        config.Section("GAMEPLAY").Key("vote_enabled").MustBool(true),
		ConsoleEnabled:     config.Section("MISC").Key("console_enabled").MustBool(true),
		LANOnly:            config.Section("NETWORK").Key("lan_only_cluster").MustBool(false),
		Offline:            config.Section("NETWORK").Key("offline_cluster").MustBool(false),
	}
	unknown := 0
	for _, section := range config.Sections() {
		for _, key := range section.Keys() {
			if !roomKnownKeys[section.Name()+"\x00"+key.Name()] {
				unknown++
			}
		}
	}
	return roomDocument{config: config, data: data, mode: mode, modified: modified, revision: revision(revisionPart{name: "cluster.ini", data: data, exists: exists}), values: values, unknown: unknown}, nil
}

func roomConfigFromDocument(document roomDocument) RoomConfig {
	return RoomConfig{Revision: document.revision, Values: document.values, Schema: append([]FieldSchema(nil), roomSchema...), UnknownFieldCount: document.unknown, ModifiedAt: document.modified}
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
	var output bytes.Buffer
	if _, err := config.WriteTo(&output); err != nil {
		return nil, nil, err
	}
	changes := roomChanges(document.values, values)
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
		{"NETWORK", "lan_only_cluster", strconv.FormatBool(after.LANOnly), before.LANOnly != after.LANOnly},
		{"NETWORK", "offline_cluster", strconv.FormatBool(after.Offline), before.Offline != after.Offline},
		{"GAMEPLAY", "game_mode", after.GameMode, before.GameMode != after.GameMode},
		{"GAMEPLAY", "max_players", strconv.Itoa(after.MaxPlayers), before.MaxPlayers != after.MaxPlayers},
		{"GAMEPLAY", "pvp", strconv.FormatBool(after.PvP), before.PvP != after.PvP},
		{"GAMEPLAY", "pause_when_empty", strconv.FormatBool(after.PauseWhenEmpty), before.PauseWhenEmpty != after.PauseWhenEmpty},
		{"GAMEPLAY", "vote_enabled", strconv.FormatBool(after.VoteEnabled), before.VoteEnabled != after.VoteEnabled},
		{"MISC", "console_enabled", strconv.FormatBool(after.ConsoleEnabled), before.ConsoleEnabled != after.ConsoleEnabled},
	}
	for _, entry := range entries {
		if !entry.changed {
			continue
		}
		config.Section(entry.section).Key(entry.key).SetValue(entry.value)
	}
}

func roomChanges(before, after RoomValues) []Change {
	valuesBefore := map[string]interface{}{
		"clusterName": before.ClusterName, "clusterDescription": before.ClusterDescription, "clusterPassword": before.ClusterPassword,
		"gameMode": before.GameMode, "maxPlayers": before.MaxPlayers, "pvp": before.PvP, "pauseWhenEmpty": before.PauseWhenEmpty,
		"voteEnabled": before.VoteEnabled, "consoleEnabled": before.ConsoleEnabled, "lanOnly": before.LANOnly, "offline": before.Offline,
	}
	valuesAfter := map[string]interface{}{
		"clusterName": after.ClusterName, "clusterDescription": after.ClusterDescription, "clusterPassword": after.ClusterPassword,
		"gameMode": after.GameMode, "maxPlayers": after.MaxPlayers, "pvp": after.PvP, "pauseWhenEmpty": after.PauseWhenEmpty,
		"voteEnabled": after.VoteEnabled, "consoleEnabled": after.ConsoleEnabled, "lanOnly": after.LANOnly, "offline": after.Offline,
	}
	keys := make([]string, 0, len(valuesBefore))
	for key := range valuesBefore {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changes := make([]Change, 0)
	for _, key := range keys {
		if reflect.DeepEqual(valuesBefore[key], valuesAfter[key]) {
			continue
		}
		change := Change{Path: "cluster." + key, Label: fieldLabel(roomSchema, key), Before: valuesBefore[key], After: valuesAfter[key], Operation: "replace"}
		if key == "clusterPassword" {
			change.Sensitive = true
			change.Before = configuredLabel(before.ClusterPassword)
			change.After = configuredLabel(after.ClusterPassword)
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
