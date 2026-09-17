package mods

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/backups"
	"dont/internal/roomops"
	"dont/internal/rooms"
)

type testRoomCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c *testRoomCatalog) List() ([]rooms.Room, error) {
	return []rooms.Room{c.room}, nil
}

func (c *testRoomCatalog) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}

func (c *testRoomCatalog) World(roomID, worldID string) (rooms.World, error) {
	if roomID != c.room.ID {
		return rooms.World{}, rooms.ErrRoomNotFound
	}
	for _, world := range c.worlds {
		if world.ID == worldID {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

func (c *testRoomCatalog) Worlds(roomID string) ([]rooms.World, error) {
	if roomID != c.room.ID {
		return nil, rooms.ErrRoomNotFound
	}
	return append([]rooms.World(nil), c.worlds...), nil
}

type testRuntime struct{}

func (testRuntime) IsRunning(context.Context, string, string) (bool, error) { return false, nil }

type testBackups struct {
	count int
}

func (b *testBackups) Create(ctx context.Context, roomID, name string, kind backups.Kind, jobID string) (backups.Backup, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, release, err := roomops.Acquire(acquireCtx, roomID)
	if err != nil {
		return backups.Backup{}, err
	}
	defer release()
	b.count++
	return backups.Backup{ID: "backup-id", RoomID: roomID, Name: name, Kind: kind, SourceJobID: jobID}, nil
}

type noOpRunner struct{}

func (noOpRunner) Download(context.Context, []string, bool, io.Writer) error { return nil }

func newConfigTestService(t *testing.T) (*Service, *testBackups, string) {
	t.Helper()
	root := t.TempDir()
	saveRoot := filepath.Join(root, "save")
	serverRoot := filepath.Join(root, "server")
	workshopRoot := filepath.Join(root, "steamapps", "workshop", "content", "322330")
	ugcRoot := filepath.Join(root, "ugc")
	worldPath := filepath.Join(saveRoot, "Cluster_1", "Master")
	modPath := filepath.Join(workshopRoot, "378160973")
	for _, path := range []string{worldPath, filepath.Join(serverRoot, "mods"), modPath, ugcRoot} {
		if err := os.MkdirAll(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	modInfo := `name = ChooseTranslationTable({zh = "全球定位", [1] = "Global Positions"})
configuration_options = {
  {name = "show_players", label = "显示玩家", options = {
    {description = "启用", data = true}, {description = "关闭", data = false},
  }, default = true},
  {name = "position_color", label = "位置颜色", options = {
    {description = "森林绿", data = "green"}, {description = "暖橙", data = "orange"},
  }, default = "green"},
}`
	if err := os.WriteFile(filepath.Join(modPath, "modinfo.lua"), []byte(modInfo), 0640); err != nil {
		t.Fatal(err)
	}
	overrides := `return {
  ["workshop-378160973"] = {
    enabled = true,
    configuration_options = {
      show_players = false,
      mystery = {nested = "keep", flags = {true, false}},
    },
    future_field = "keep-me",
  },
  operator_value = {untouched = true},
}`
	if err := os.WriteFile(filepath.Join(worldPath, "modoverrides.lua"), []byte(overrides), 0640); err != nil {
		t.Fatal(err)
	}
	catalog := &testRoomCatalog{
		room:   rooms.Room{ID: "room-1", DirectoryName: "Cluster_1", Name: "测试房间", Managed: true},
		worlds: []rooms.World{{ID: "world-1", RoomID: "room-1", DirectoryName: "Master", Name: "地面", IsMaster: true}},
	}
	backupService := &testBackups{}
	service, err := NewService(Config{
		SaveRoot: saveRoot, ServerRoot: serverRoot, WorkshopContentRoot: workshopRoot,
		UGCRoot: ugcRoot, AppID: "322330",
	}, catalog, testRuntime{}, backupService, NewMemoryMetadataProvider(), NewDualParser("", ""), noOpRunner{})
	if err != nil {
		t.Fatal(err)
	}
	return service, backupService, filepath.Join(worldPath, "modoverrides.lua")
}

func TestConfigurationPreviewApplyPreservesUnknownFields(t *testing.T) {
	service, backupService, overridesPath := newConfigTestService(t)
	ctx := context.Background()
	configuration, err := service.Configuration(ctx, "room-1", "world-1", "378160973")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Parser != "go" || configuration.FallbackUsed || len(configuration.Fields) != 2 {
		t.Fatalf("unexpected schema: %#v", configuration)
	}
	if configuration.Values["show_players"] != false {
		t.Fatalf("current value not loaded: %#v", configuration.Values)
	}
	if configuration.Overrides["show_players"] != false {
		t.Fatalf("explicit override not reported: %#v", configuration.Overrides)
	}
	if _, overridden := configuration.Overrides["position_color"]; overridden {
		t.Fatalf("default-only value reported as override: %#v", configuration.Overrides)
	}
	mystery, ok := configuration.UnknownValues["mystery"].(map[string]interface{})
	if !ok || mystery["nested"] != "keep" {
		t.Fatalf("unknown nested value missing: %#v", configuration.UnknownValues)
	}
	request := ConfigUpdateRequest{
		ExpectedRevision: configuration.Revision,
		Enabled:          false,
		Patch: map[string]json.RawMessage{
			"position_color": json.RawMessage(`"orange"`),
		},
	}
	preview, err := service.PreviewConfiguration(ctx, "room-1", "world-1", "378160973", request)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 2 || preview.NextRevision == preview.Revision || !preview.RawPreserved {
		t.Fatalf("unexpected preview: %#v", preview)
	}
	result, err := service.ApplyConfiguration(ctx, "job-1", "room-1", "world-1", "378160973", request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != preview.NextRevision || backupService.count != 0 || result.ProtectionBackupID != "" {
		t.Fatalf("unexpected apply result: %#v, backups=%d", result, backupService.count)
	}
	next, err := service.Configuration(ctx, "room-1", "world-1", "378160973")
	if err != nil {
		t.Fatal(err)
	}
	if next.Enabled || next.Values["position_color"] != "orange" {
		t.Fatalf("applied values missing: %#v", next)
	}
	mystery, ok = next.UnknownValues["mystery"].(map[string]interface{})
	if !ok || mystery["nested"] != "keep" {
		t.Fatalf("unknown nested value was lost: %#v", next.UnknownValues)
	}
	rendered, err := os.ReadFile(overridesPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"future_field", "operator_value", "mystery", "flags"} {
		if !strings.Contains(string(rendered), expected) {
			t.Fatalf("unknown field %q was lost:\n%s", expected, rendered)
		}
	}

	if _, err := service.ApplyConfiguration(ctx, "job-2", "room-1", "world-1", "378160973", request); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
	noChange := ConfigUpdateRequest{ExpectedRevision: next.Revision, Enabled: false, Patch: map[string]json.RawMessage{"position_color": json.RawMessage(`"orange"`)}}
	if _, err := service.ApplyConfiguration(ctx, "job-3", "room-1", "world-1", "378160973", noChange); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("expected no changes, got %v", err)
	}
	if backupService.count != 0 {
		t.Fatalf("no-change request created a backup: %d", backupService.count)
	}
}

func TestConfigurationFileReturnsExactWorldFile(t *testing.T) {
	service, _, overridesPath := newConfigTestService(t)
	want, err := os.ReadFile(overridesPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ConfigurationFile("room-1", "world-1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Exists || result.FileName != "modoverrides.lua" || result.Content != string(want) || result.Revision == "" {
		t.Fatalf("unexpected configuration file: %#v", result)
	}
}

func TestPlacementContentDoesNotRequireLocalWorldDirectory(t *testing.T) {
	service, _, overridesPath := newConfigTestService(t)
	content, err := os.ReadFile(overridesPath)
	if err != nil {
		t.Fatal(err)
	}
	service.rooms.(*testRoomCatalog).worlds = nil

	configuration, err := service.ConfigurationFromContent(context.Background(), "room-1", "world-1", "378160973", content)
	if err != nil || configuration.ModID != "378160973" || len(configuration.Fields) != 2 {
		t.Fatalf("configuration=%#v err=%v", configuration, err)
	}
	list, err := service.ListFromOverrides(context.Background(), "room-1", map[string][]byte{"world-1": content})
	if err != nil || list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != "378160973" {
		t.Fatalf("list=%#v err=%v", list, err)
	}
}

func TestConfigurationRejectsValueOutsideDeclaredOptions(t *testing.T) {
	service, backupService, _ := newConfigTestService(t)
	configuration, err := service.Configuration(context.Background(), "room-1", "world-1", "378160973")
	if err != nil {
		t.Fatal(err)
	}
	request := ConfigUpdateRequest{ExpectedRevision: configuration.Revision, Enabled: true, Patch: map[string]json.RawMessage{"position_color": json.RawMessage(`"blue"`)}}
	_, err = service.ApplyConfiguration(context.Background(), "job", "room-1", "world-1", "378160973", request)
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Fields["patch.position_color"] == "" {
		t.Fatalf("expected field error, got %v", err)
	}
	if backupService.count != 0 {
		t.Fatalf("invalid request created a backup: %d", backupService.count)
	}
}

func TestConfigurationRestoreDefaultsRemovesKnownOverrides(t *testing.T) {
	service, _, overridesPath := newConfigTestService(t)
	ctx := context.Background()
	configuration, err := service.Configuration(ctx, "room-1", "world-1", "378160973")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ApplyConfiguration(ctx, "job-restore", "room-1", "world-1", "378160973", ConfigUpdateRequest{
		ExpectedRevision: configuration.Revision,
		Enabled:          true,
		Patch: map[string]json.RawMessage{
			"show_players": json.RawMessage(`null`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.Configuration(ctx, "room-1", "world-1", "378160973")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Values["show_players"] != true {
		t.Fatalf("declared default was not restored: %#v", restored.Values)
	}
	if _, overridden := restored.Overrides["show_players"]; overridden {
		t.Fatalf("known override was not removed: %#v", restored.Overrides)
	}
	if _, preserved := restored.UnknownValues["mystery"]; !preserved {
		t.Fatalf("unknown override was removed: %#v", restored.UnknownValues)
	}
	rendered, err := os.ReadFile(overridesPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "show_players") || !strings.Contains(string(rendered), "mystery") {
		t.Fatalf("restored file did not preserve the expected fields:\n%s", rendered)
	}
}

func TestWorldModConfigurationsRemainIndependent(t *testing.T) {
	service, _, _ := newConfigTestService(t)
	catalog := service.rooms.(*testRoomCatalog)
	caves := rooms.World{ID: "world-2", RoomID: "room-1", DirectoryName: "Caves", Name: "洞穴"}
	catalog.worlds = append(catalog.worlds, caves)
	cavesPath := filepath.Join(service.config.SaveRoot, catalog.room.DirectoryName, caves.DirectoryName)
	if err := os.MkdirAll(cavesPath, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cavesPath, "modoverrides.lua"), []byte(`return {
  ["workshop-378160973"] = {
    enabled = true,
    configuration_options = { show_players = true },
  },
}`), 0640); err != nil {
		t.Fatal(err)
	}
	master, err := service.Configuration(context.Background(), "room-1", "world-1", "378160973")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ApplyConfiguration(context.Background(), "job", "room-1", "world-1", "378160973", ConfigUpdateRequest{
		ExpectedRevision: master.Revision,
		Enabled:          false,
		Patch:            map[string]json.RawMessage{"show_players": json.RawMessage(`false`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	cavesConfig, err := service.Configuration(context.Background(), "room-1", "world-2", "378160973")
	if err != nil {
		t.Fatal(err)
	}
	if !cavesConfig.Enabled || cavesConfig.Values["show_players"] != true {
		t.Fatalf("editing Master leaked into Caves: %#v", cavesConfig)
	}
}

func TestEnableNoChangeDoesNotCreateBackup(t *testing.T) {
	service, backupService, _ := newConfigTestService(t)
	_, err := service.Enable(context.Background(), "job", "room-1", "378160973", EnableRequest{WorldIDs: []string{"world-1"}, Enabled: true})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("expected no changes, got %v", err)
	}
	if backupService.count != 0 {
		t.Fatalf("no-change enable created a backup: %d", backupService.count)
	}
}

func TestEnableChangesOnlyModOverridesWithoutBackup(t *testing.T) {
	service, backupService, overridesPath := newConfigTestService(t)
	result, err := service.Enable(context.Background(), "job", "room-1", "378160973", EnableRequest{
		WorldIDs: []string{"world-1"}, Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtectionBackupID != "" || backupService.count != 0 {
		t.Fatalf("enable result=%#v backups=%d", result, backupService.count)
	}
	content, err := os.ReadFile(overridesPath)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := InspectModOverride(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Mods) != 1 || configuration.Mods[0].Enabled {
		t.Fatalf("enabled flag was not updated: %#v", configuration.Mods)
	}
}
