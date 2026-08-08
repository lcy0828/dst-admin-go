package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dont/internal/backups"
	"dont/internal/rooms"
)

type configurationCatalog struct {
	room  rooms.Room
	world rooms.World
}

func (c configurationCatalog) Room(string) (rooms.Room, error)        { return c.room, nil }
func (c configurationCatalog) World(_, _ string) (rooms.World, error) { return c.world, nil }

type configurationBackups struct {
	count    int
	onCreate func()
	err      error
}

func (b *configurationBackups) Create(_ context.Context, roomID, _ string, kind backups.Kind, jobID string) (backups.Backup, error) {
	b.count++
	if b.onCreate != nil {
		b.onCreate()
	}
	if b.err != nil {
		return backups.Backup{}, b.err
	}
	if kind != backups.KindProtection || roomID != "room" || jobID == "" {
		return backups.Backup{}, errors.New("invalid protection backup request")
	}
	return backups.Backup{ID: "backup-id", RoomID: roomID, Kind: kind, SourceJobID: jobID}, nil
}

func newConfigurationService(t *testing.T) (*Service, string, *configurationBackups) {
	t.Helper()
	root := t.TempDir()
	roomPath := filepath.Join(root, "Cluster")
	worldPath := filepath.Join(roomPath, "Master")
	if err := os.MkdirAll(worldPath, 0750); err != nil {
		t.Fatal(err)
	}
	cluster := "[NETWORK]\ncluster_name = Original\ncluster_description = Test\ncluster_password = old-secret\n[CUSTOM]\nkeep = yes\n[GAMEPLAY]\ngame_mode = survival\nmax_players = 6\npvp = false\n"
	server := "[NETWORK]\nserver_port = 10999\n[SHARD]\nis_master = true\nname = Master\nid = 1\n[STEAM]\nauthentication_port = 8768\nmaster_server_port = 27018\n[ACCOUNT]\nencode_user_path = true\n[CUSTOM]\nkeep = yes\n"
	override := `return {
  location = "forest",
  overrides = {
    day = "default",
    complex_unknown = { [1] = "first", enabled = true },
  },
  unknown_outer = { [false] = "kept", [7] = "seven" },
}` + "\n"
	for name, data := range map[string]string{
		filepath.Join(roomPath, "cluster.ini"):            cluster,
		filepath.Join(worldPath, "server.ini"):            server,
		filepath.Join(worldPath, "leveldataoverride.lua"): override,
	} {
		if err := os.WriteFile(name, []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	backup := &configurationBackups{}
	catalog := configurationCatalog{
		room:  rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Original", Managed: true},
		world: rooms.World{ID: "world", RoomID: "room", DirectoryName: "Master", Name: "Master", IsMaster: true},
	}
	service, err := NewService(root, catalog, backup)
	if err != nil {
		t.Fatal(err)
	}
	return service, roomPath, backup
}

func TestRoomConfigurationPreviewApplyPreservesUnknownAndProtectsSecrets(t *testing.T) {
	service, roomPath, backup := newConfigurationService(t)
	current, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	if current.UnknownFieldCount != 1 || current.Values.ClusterName != "Original" {
		t.Fatalf("config = %#v", current)
	}
	next := current.Values
	next.ClusterName = "Changed"
	next.ClusterPassword = "new-secret"
	request := RoomUpdateRequest{ExpectedRevision: current.Revision, Values: next}
	preview, err := service.PreviewRoom("room", request)
	if err != nil {
		t.Fatal(err)
	}
	if preview.NextRevision == current.Revision || len(preview.Changes) != 2 {
		t.Fatalf("preview = %#v", preview)
	}
	encoded, _ := json.Marshal(preview)
	if strings.Contains(string(encoded), "old-secret") || strings.Contains(string(encoded), "new-secret") {
		t.Fatal("preview leaked a room password")
	}
	result, err := service.ApplyRoom(context.Background(), "job-1", "room", request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != preview.NextRevision || result.ProtectionBackupID != "backup-id" || backup.count != 1 {
		t.Fatalf("result = %#v, backups = %d", result, backup.count)
	}
	written, err := os.ReadFile(filepath.Join(roomPath, "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "keep = yes") || !strings.Contains(string(written), "cluster_name") || !strings.Contains(string(written), "Changed") {
		t.Fatalf("cluster.ini = %s", written)
	}
	if _, err := service.ApplyRoom(context.Background(), "job-2", "room", request); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func TestRoomConfigurationRoundTripsEveryLegacyField(t *testing.T) {
	service, _, _ := newConfigurationService(t)
	current, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	next := RoomValues{
		ClusterName:        "Complete room",
		ClusterDescription: "Every legacy setting",
		ClusterPassword:    "room-secret",
		ClusterIntention:   "social",
		ClusterLanguage:    "en",
		GameMode:           "endless",
		MaxPlayers:         18,
		PvP:                true,
		PauseWhenEmpty:     false,
		VoteEnabled:        false,
		VoteKickEnabled:    true,
		ConsoleEnabled:     false,
		LANOnly:            true,
		Offline:            true,
		WhitelistSlots:     4,
		TickRate:           30,
		AutosaverEnabled:   false,
		IdleTimeout:        120,
		MaxSnapshots:       20,
		ShardEnabled:       false,
		BindIP:             "0.0.0.0",
		MasterIP:           "192.168.2.12",
		MasterPort:         10888,
		ClusterKey:         "shard-secret",
		SteamGroupOnly:     true,
		SteamGroupID:       123456789,
		SteamGroupAdmins:   true,
	}
	request := RoomUpdateRequest{ExpectedRevision: current.Revision, Values: next}
	preview, err := service.PreviewRoom("room", request)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != len(roomSchema) {
		t.Fatalf("changes = %d, schema fields = %d", len(preview.Changes), len(roomSchema))
	}
	if _, err := service.ApplyRoom(context.Background(), "job-all-room-fields", "room", request); err != nil {
		t.Fatal(err)
	}
	updated, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.Values, next) {
		t.Fatalf("round trip mismatch:\nwant %#v\n got %#v", next, updated.Values)
	}
}

func TestConfigurationRechecksRevisionAfterProtectionBackup(t *testing.T) {
	service, roomPath, backup := newConfigurationService(t)
	current, _ := service.RoomConfig("room")
	next := current.Values
	next.MaxPlayers = 12
	backup.onCreate = func() {
		path := filepath.Join(roomPath, "cluster.ini")
		data, _ := os.ReadFile(path)
		_ = os.WriteFile(path, append(data, []byte("\n# external edit\n")...), 0640)
	}
	_, err := service.ApplyRoom(context.Background(), "job-race", "room", RoomUpdateRequest{ExpectedRevision: current.Revision, Values: next})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("race error = %v", err)
	}
	written, _ := os.ReadFile(filepath.Join(roomPath, "cluster.ini"))
	if !strings.Contains(string(written), "external edit") || strings.Contains(string(written), "max_players = 12") {
		t.Fatalf("external edit was overwritten: %s", written)
	}
}

func TestAccessListsRequireConfirmationForRemovalAndNormalize(t *testing.T) {
	service, roomPath, backup := newConfigurationService(t)
	if err := os.WriteFile(filepath.Join(roomPath, "adminlist.txt"), []byte("KU_TWO\nKU_ONE\n"), 0640); err != nil {
		t.Fatal(err)
	}
	current, err := service.AccessLists("room")
	if err != nil {
		t.Fatal(err)
	}
	request := AccessUpdateRequest{ExpectedRevision: current.Revision, Admins: []string{"KU_ONE"}, Blocked: []string{"KU_BAD", "KU_BAD"}}
	previewWithoutConfirmation, err := service.PreviewAccess("room", request)
	if err != nil || !previewWithoutConfirmation.RequiresConfirmation {
		t.Fatalf("preview without confirmation = %#v, %v", previewWithoutConfirmation, err)
	}
	if _, err := service.ValidateAccessApply("room", request); !errors.Is(err, ErrConfirmationNeeded) {
		t.Fatalf("apply confirmation error = %v", err)
	}
	request.Confirmation = "Original"
	preview, err := service.PreviewAccess("room", request)
	if err != nil || !preview.RequiresConfirmation {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	if _, err := service.ApplyAccess(context.Background(), "job-access", "room", request); err != nil {
		t.Fatal(err)
	}
	blocked, _ := os.ReadFile(filepath.Join(roomPath, "blocklist.txt"))
	if string(blocked) != "KU_BAD\n" || backup.count != 1 {
		t.Fatalf("blocked = %q, backups = %d", blocked, backup.count)
	}
	if _, err := os.Lstat(filepath.Join(roomPath, "whitelist.txt")); !os.IsNotExist(err) {
		t.Fatalf("unchanged absent whitelist was written: %v", err)
	}
}

func TestTokenIsMaskedAndRequiresExactConfirmation(t *testing.T) {
	service, roomPath, backup := newConfigurationService(t)
	if err := os.WriteFile(filepath.Join(roomPath, "cluster_token.txt"), []byte("very-secret-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := service.TokenStatus("room")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || status.MaskedValue != "****oken" {
		t.Fatalf("status = %#v", status)
	}
	if _, err := service.RevealToken("room", "wrong"); !errors.Is(err, ErrConfirmationNeeded) {
		t.Fatalf("reveal error = %v", err)
	}
	revealed, err := service.RevealToken("room", "Original")
	if err != nil || revealed.Token != "very-secret-token" {
		t.Fatalf("reveal = %#v, %v", revealed, err)
	}
	request := TokenUpdateRequest{ExpectedRevision: status.Revision, Token: "replacement-token", Confirmation: "Original"}
	preview, err := service.PreviewToken("room", request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(preview)
	if strings.Contains(string(encoded), "replacement-token") || strings.Contains(string(encoded), "very-secret-token") {
		t.Fatal("token preview leaked the secret")
	}
	if _, err := service.ApplyToken(context.Background(), "job-token", "room", request); err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(filepath.Join(roomPath, "cluster_token.txt"))
	if string(written) != "replacement-token\n" || backup.count != 1 {
		t.Fatalf("token write failed, backups = %d", backup.count)
	}
}

func TestWorldConfigurationPreservesComplexUnknownLuaValues(t *testing.T) {
	service, roomPath, backup := newConfigurationService(t)
	current, err := service.WorldConfig("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	if current.UnknownFieldCount != 1 || current.Overrides["day"] != "default" {
		t.Fatalf("world config = %#v", current)
	}
	server := current.Server
	server.ServerPort = 11000
	server.IsMaster = false
	server.ShardName = "Forest Two"
	server.ShardID = 2
	request := WorldUpdateRequest{
		ExpectedRevision: current.Revision, Server: server,
		OverridePatch: map[string]json.RawMessage{"day": json.RawMessage(`"longday"`), "new_option": json.RawMessage(`{"enabled":true,"values":[1,2]}`)},
	}
	preview, err := service.PreviewWorld("room", "world", request)
	if err != nil || len(preview.Changes) != 6 {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	if _, err := service.ApplyWorld(context.Background(), "job-world", "room", "world", request); err != nil {
		t.Fatal(err)
	}
	updated, err := service.WorldConfig("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Server.ServerPort != 11000 || updated.Server.IsMaster || updated.Server.ShardName != "Forest Two" || updated.Server.ShardID != 2 || updated.Overrides["day"] != "longday" || backup.count != 1 {
		t.Fatalf("updated = %#v", updated)
	}
	written, _ := os.ReadFile(filepath.Join(roomPath, "Master", "leveldataoverride.lua"))
	root, err := parseLuaReturnTable(written)
	if err != nil {
		t.Fatalf("rewritten Lua is invalid: %v\n%s", err, written)
	}
	outer, ok := root.stringEntry("unknown_outer")
	if !ok || len(outer.entries) != 2 || outer.entries[0].key.kind.String() == "string" {
		t.Fatalf("unknown outer table was not preserved: %#v", outer)
	}
	complex, _ := root.stringEntry("overrides")
	complex, ok = complex.stringEntry("complex_unknown")
	if !ok || len(complex.entries) != 2 {
		t.Fatalf("complex override was not preserved: %#v", complex)
	}
}

func TestLuaConfigurationSandboxRejectsGlobalAccess(t *testing.T) {
	_, err := parseLuaReturnTable([]byte(`return os.execute("touch should-not-run")`))
	if err == nil {
		t.Fatal("sandbox allowed access to os.execute")
	}
}

func TestWorldRendererDoesNotRewriteUnchangedCompanionFile(t *testing.T) {
	_, roomPath, _ := newConfigurationService(t)
	worldPath := filepath.Join(roomPath, "Master")
	document, err := loadWorldDocument(worldPath)
	if err != nil {
		t.Fatal(err)
	}
	serverOnly := WorldUpdateRequest{ExpectedRevision: document.revision, Server: document.serverValues}
	serverOnly.Server.ServerPort = 11001
	_, luaData, _, err := renderWorldDocument(document, serverOnly)
	if err != nil {
		t.Fatal(err)
	}
	if string(luaData) != string(document.luaData) {
		t.Fatal("server-only update rewrote leveldataoverride.lua")
	}
	overrideOnly := WorldUpdateRequest{
		ExpectedRevision: document.revision, Server: document.serverValues,
		OverridePatch: map[string]json.RawMessage{"day": json.RawMessage(`"onlyday"`)},
	}
	serverData, _, _, err := renderWorldDocument(document, overrideOnly)
	if err != nil {
		t.Fatal(err)
	}
	if string(serverData) != string(document.serverData) {
		t.Fatal("override-only update rewrote server.ini")
	}
	invalidNestedNull := overrideOnly
	invalidNestedNull.OverridePatch = map[string]json.RawMessage{"bad": json.RawMessage(`[1,null]`)}
	if _, _, _, err := renderWorldDocument(document, invalidNestedNull); err == nil {
		t.Fatal("nested JSON null was accepted")
	}
}
