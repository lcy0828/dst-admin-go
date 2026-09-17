package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dont/internal/backups"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/shared"
)

type configurationCatalog struct {
	room  rooms.Room
	world rooms.World
}

func (c configurationCatalog) Room(string) (rooms.Room, error)        { return c.room, nil }
func (c configurationCatalog) World(_, _ string) (rooms.World, error) { return c.world, nil }
func (c configurationCatalog) Worlds(string) ([]rooms.World, error) {
	return []rooms.World{c.world}, nil
}

type configurationSnapshotReader struct {
	files []shared.RuntimeConfigurationFile
}

type routedConfigurationReader struct {
	results map[string]shared.RuntimeConfigurationResult
	token   shared.RuntimeClusterTokenReveal
}

func (r routedConfigurationReader) ReadConfiguration(_ context.Context, _, _, scope string) (runtimedriver.ConfigurationSnapshot, error) {
	result, ok := r.results[scope]
	if !ok {
		return runtimedriver.ConfigurationSnapshot{}, errors.New("unexpected configuration scope")
	}
	return runtimedriver.ConfigurationSnapshot{Target: runtimedriver.Target{TargetID: "agent:remote"}, Result: result}, nil
}

func (r routedConfigurationReader) RevealClusterToken(context.Context, string, string) (shared.RuntimeClusterTokenReveal, error) {
	return r.token, nil
}

func routedConfigurationFile(name, data string, mode os.FileMode, modified time.Time) shared.RuntimeConfigurationFile {
	digest := sha256.Sum256([]byte(data))
	return shared.RuntimeConfigurationFile{
		Name: name, Exists: true, Mode: uint32(mode.Perm()), Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), UpdatedAt: modified, Data: []byte(data),
	}
}

func (r configurationSnapshotReader) ReadConfiguration(context.Context, string, string, string) (runtimedriver.ConfigurationSnapshot, error) {
	return runtimedriver.ConfigurationSnapshot{
		Target: runtimedriver.Target{TargetID: "agent:remote"},
		Result: shared.RuntimeConfigurationResult{Complete: true, Files: append([]shared.RuntimeConfigurationFile(nil), r.files...)},
	}, nil
}

type configurationBackups struct {
	count    int
	onCreate func()
	err      error
}

type configurationPublisher struct {
	requests []PublicationRequest
	count    int
	err      error
}

func (p *configurationPublisher) Publish(_ context.Context, request PublicationRequest) (PublicationResult, error) {
	p.requests = append(p.requests, request)
	p.count++
	return PublicationResult{PublicationID: "publication-id", PublishedCount: p.count}, p.err
}

func (b *configurationBackups) Create(ctx context.Context, roomID, _ string, kind backups.Kind, jobID string) (backups.Backup, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, release, err := roomops.Acquire(acquireCtx, roomID)
	if err != nil {
		return backups.Backup{}, err
	}
	defer release()
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
	cluster := "[NETWORK]\ncluster_name = Original\ncluster_description = Test\ncluster_password = old-secret\ncluster_language = zh\n[CUSTOM]\nkeep = yes\n[GAMEPLAY]\ngame_mode = survival\nmax_players = 6\npvp = false\n"
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
	if result.Revision != preview.NextRevision || result.ProtectionBackupID != "" || backup.count != 0 {
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

func TestRoomConfigurationMaterializesMissingDefaultLanguage(t *testing.T) {
	service, roomPath, _ := newConfigurationService(t)
	path := filepath.Join(roomPath, "cluster.ini")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "cluster_language = zh\n", ""))
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "cluster_language") {
		t.Fatalf("test fixture still contains cluster_language:\n%s", data)
	}

	current, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	if current.Values.ClusterLanguage != "zh" {
		t.Fatalf("default cluster language = %q", current.Values.ClusterLanguage)
	}
	request := RoomUpdateRequest{ExpectedRevision: current.Revision, Values: current.Values}
	preview, err := service.PreviewRoom("room", request)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 1 {
		t.Fatalf("preview changes = %#v", preview.Changes)
	}
	change := preview.Changes[0]
	if change.Path != "cluster.clusterLanguage" || change.Operation != "add" || change.Before != nil || change.After != "zh" {
		t.Fatalf("language change = %#v", change)
	}

	result, err := service.ApplyRoom(context.Background(), "job-materialize-language", "room", request)
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "cluster_language") || !strings.Contains(string(written), "= zh") {
		t.Fatalf("cluster.ini does not contain materialized language:\n%s", written)
	}

	updated, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != result.Revision {
		t.Fatalf("revision mismatch: config=%q result=%q", updated.Revision, result.Revision)
	}
	if _, err := service.PreviewRoom("room", RoomUpdateRequest{ExpectedRevision: updated.Revision, Values: updated.Values}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("unchanged materialized language error = %v", err)
	}

	english := updated.Values
	english.ClusterLanguage = "en"
	englishPreview, err := service.PreviewRoom("room", RoomUpdateRequest{ExpectedRevision: updated.Revision, Values: english})
	if err != nil {
		t.Fatal(err)
	}
	if len(englishPreview.Changes) != 1 || englishPreview.Changes[0].Operation != "replace" {
		t.Fatalf("english preview changes = %#v", englishPreview.Changes)
	}
	if _, err := service.ApplyRoom(context.Background(), "job-language-english", "room", RoomUpdateRequest{ExpectedRevision: updated.Revision, Values: english}); err != nil {
		t.Fatal(err)
	}
	englishConfig, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	if englishConfig.Values.ClusterLanguage != "en" {
		t.Fatalf("saved English language = %q", englishConfig.Values.ClusterLanguage)
	}
	chinese := englishConfig.Values
	chinese.ClusterLanguage = "zh"
	if _, err := service.ApplyRoom(context.Background(), "job-language-chinese", "room", RoomUpdateRequest{ExpectedRevision: englishConfig.Revision, Values: chinese}); err != nil {
		t.Fatal(err)
	}
	chineseConfig, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	if chineseConfig.Values.ClusterLanguage != "zh" {
		t.Fatalf("saved Chinese language = %q", chineseConfig.Values.ClusterLanguage)
	}
}

func TestConfigurationRejectsExternalEditBeforeApply(t *testing.T) {
	service, roomPath, _ := newConfigurationService(t)
	current, _ := service.RoomConfig("room")
	next := current.Values
	next.MaxPlayers = 12
	path := filepath.Join(roomPath, "cluster.ini")
	data, _ := os.ReadFile(path)
	externallyEdited := append(append([]byte(nil), data...), []byte("\n[MANUAL]\noperator_value = keep\n")...)
	_ = os.WriteFile(path, externallyEdited, 0640)
	_, err := service.ApplyRoom(context.Background(), "job-race", "room", RoomUpdateRequest{ExpectedRevision: current.Revision, Values: next})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("race error = %v", err)
	}
	written, _ := os.ReadFile(filepath.Join(roomPath, "cluster.ini"))
	if string(written) != string(externallyEdited) || strings.Contains(string(written), "max_players = 12") {
		t.Fatalf("external edit was overwritten: %s", written)
	}
	latest, err := service.RoomConfig("room")
	if err != nil || latest.Revision == current.Revision || latest.UnknownFieldCount != current.UnknownFieldCount+1 {
		t.Fatalf("external room edit was not visible on reread: config=%#v err=%v", latest, err)
	}
}

func TestRoomConfigurationRollsBackLocalFileWhenRemotePublicationFails(t *testing.T) {
	service, roomPath, _ := newConfigurationService(t)
	publisher := &configurationPublisher{err: errors.New("remote publication failed")}
	if err := service.ConfigurePublisher(publisher); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(roomPath, "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	next := current.Values
	next.MaxPlayers = 12
	_, err = service.ApplyRoom(context.Background(), "job-publication-failure", "room", RoomUpdateRequest{ExpectedRevision: current.Revision, Values: next})
	if err == nil || !strings.Contains(err.Error(), "remote publication failed") {
		t.Fatalf("ApplyRoom error=%v", err)
	}
	after, err := os.ReadFile(filepath.Join(roomPath, "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("local configuration was not rolled back:\n%s", after)
	}
	if len(publisher.requests) != 1 || publisher.requests[0].Scope != PublicationShared || !reflect.DeepEqual(publisher.requests[0].Files, []string{"cluster.ini"}) {
		t.Fatalf("publication requests=%#v", publisher.requests)
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
	if string(blocked) != "KU_BAD\n" || backup.count != 0 {
		t.Fatalf("blocked = %q, backups = %d", blocked, backup.count)
	}
	if _, err := os.Lstat(filepath.Join(roomPath, "whitelist.txt")); !os.IsNotExist(err) {
		t.Fatalf("unchanged absent whitelist was written: %v", err)
	}
}

func TestAccessListsReadAndPublishThroughRuntimePlacement(t *testing.T) {
	service, _, backup := newConfigurationService(t)
	publisher := &configurationPublisher{}
	if err := service.ConfigurePublisher(publisher); err != nil {
		t.Fatal(err)
	}
	reader := configurationSnapshotReader{files: []shared.RuntimeConfigurationFile{
		{Name: "cluster.ini", Exists: true, Mode: 0o640, Data: []byte("[NETWORK]\ncluster_name=Remote\n")},
		{Name: "adminlist.txt"},
		{Name: "blocklist.txt"},
		{Name: "whitelist.txt"},
	}}
	if err := service.ConfigureReader(reader); err != nil {
		t.Fatal(err)
	}
	current, err := service.AccessLists("room")
	if err != nil {
		t.Fatal(err)
	}
	request := AccessUpdateRequest{ExpectedRevision: current.Revision, Blocked: []string{"KU_REMOTE"}}
	result, err := service.ApplyAccess(context.Background(), "job-runtime-access", "room", request)
	if err != nil {
		t.Fatal(err)
	}
	if backup.count != 0 || result.PublishedTargets != 1 || len(publisher.requests) != 1 {
		t.Fatalf("result=%#v backups=%d requests=%#v", result, backup.count, publisher.requests)
	}
	publication := publisher.requests[0]
	if !publication.IncludeLocal || len(publication.Payload) != 1 || publication.Payload[0].Name != "blocklist.txt" || string(publication.Payload[0].Data) != "KU_REMOTE\n" {
		t.Fatalf("publication=%#v", publication)
	}
}

func TestRoomWorldAndTokenUseRuntimePlacementWithoutChangingControllerFiles(t *testing.T) {
	service, roomPath, backup := newConfigurationService(t)
	publisher := &configurationPublisher{}
	if err := service.ConfigurePublisher(publisher); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().UTC().Truncate(time.Second)
	remoteRoom := "[NETWORK]\ncluster_name = Remote\ncluster_language = zh\n[GAMEPLAY]\ngame_mode = survival\nmax_players = 8\n"
	remoteServer := "[NETWORK]\nserver_port = 11000\n[SHARD]\nis_master = true\nname = Remote Master\nid = 1\n[STEAM]\nauthentication_port = 8769\nmaster_server_port = 27019\n[ACCOUNT]\nencode_user_path = true\n"
	remoteOverride := "return { overrides = { day = \"default\" } }\n"
	token := "remote-secret-token"
	tokenDigest := sha256.Sum256([]byte(token))
	reader := routedConfigurationReader{
		results: map[string]shared.RuntimeConfigurationResult{
			"shared": {Complete: true, Files: []shared.RuntimeConfigurationFile{
				routedConfigurationFile("cluster.ini", remoteRoom, 0o640, modified),
				{Name: "adminlist.txt"}, {Name: "blocklist.txt"}, {Name: "whitelist.txt"},
			}},
			"world": {Complete: true, Files: []shared.RuntimeConfigurationFile{
				routedConfigurationFile("server.ini", remoteServer, 0o640, modified),
				routedConfigurationFile("leveldataoverride.lua", remoteOverride, 0o640, modified),
			}},
			"token-status": {Complete: true, TokenStatus: &shared.RuntimeClusterTokenStatus{
				Exists: true, Configured: true, MaskedValue: "****oken", SHA256: hex.EncodeToString(tokenDigest[:]), Mode: 0o600, UpdatedAt: modified,
			}},
		},
		token: shared.RuntimeClusterTokenReveal{Exists: true, Token: token, SHA256: hex.EncodeToString(tokenDigest[:]), UpdatedAt: modified},
	}
	if err := service.ConfigureReader(reader); err != nil {
		t.Fatal(err)
	}

	roomConfig, err := service.RoomConfig("room")
	if err != nil || roomConfig.Values.ClusterName != "Remote" || roomConfig.Values.MaxPlayers != 8 {
		t.Fatalf("room config = %#v, %v", roomConfig, err)
	}
	roomValues := roomConfig.Values
	roomValues.MaxPlayers = 10
	roomRequest := RoomUpdateRequest{ExpectedRevision: roomConfig.Revision, Values: roomValues}
	if _, err := service.ApplyRoom(context.Background(), "job-runtime-room", "room", roomRequest); err != nil {
		t.Fatal(err)
	}
	roomPublication := publisher.requests[len(publisher.requests)-1]
	if !roomPublication.IncludeLocal || len(roomPublication.Payload) != 1 || !strings.Contains(string(roomPublication.Payload[0].Data), "max_players = 10") {
		t.Fatalf("room publication = %#v", roomPublication)
	}

	worldConfig, err := service.WorldConfig("room", "world")
	if err != nil || worldConfig.Server.ServerPort != 11000 || worldConfig.Server.ShardName != "Remote Master" {
		t.Fatalf("world config = %#v, %v", worldConfig, err)
	}
	worldServer := worldConfig.Server
	worldServer.ServerPort = 11001
	worldRequest := WorldUpdateRequest{ExpectedRevision: worldConfig.Revision, Server: worldServer}
	if _, err := service.ApplyWorld(context.Background(), "job-runtime-world", "room", "world", worldRequest); err != nil {
		t.Fatal(err)
	}
	worldPublication := publisher.requests[len(publisher.requests)-1]
	if !worldPublication.IncludeLocal || len(worldPublication.Payload) != 2 || !strings.Contains(string(worldPublication.Payload[0].Data), "11001") && !strings.Contains(string(worldPublication.Payload[1].Data), "11001") {
		t.Fatalf("world publication = %#v", worldPublication)
	}

	status, err := service.TokenStatus("room")
	if err != nil || !status.Configured || status.MaskedValue != "****oken" {
		t.Fatalf("token status = %#v, %v", status, err)
	}
	revealed, err := service.RevealToken("room", "Original")
	if err != nil || revealed.Token != token {
		t.Fatalf("token reveal = %#v, %v", revealed, err)
	}
	tokenRequest := TokenUpdateRequest{ExpectedRevision: status.Revision, Token: "replacement-token", Confirmation: "Original"}
	if _, err := service.ApplyToken(context.Background(), "job-runtime-token", "room", tokenRequest); err != nil {
		t.Fatal(err)
	}
	tokenPublication := publisher.requests[len(publisher.requests)-1]
	if !tokenPublication.IncludeLocal || len(tokenPublication.Payload) != 1 || string(tokenPublication.Payload[0].Data) != "replacement-token\n" {
		t.Fatalf("token publication = %#v", tokenPublication)
	}

	localRoom, err := os.ReadFile(filepath.Join(roomPath, "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(localRoom), "Remote") || backup.count != 0 {
		t.Fatalf("controller file was changed or backup count is wrong: backups=%d\n%s", backup.count, localRoom)
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
	if string(written) != "replacement-token\n" || backup.count != 0 {
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
	if updated.Server.ServerPort != 11000 || updated.Server.IsMaster || updated.Server.ShardName != "Forest Two" || updated.Server.ShardID != 2 || updated.Overrides["day"] != "longday" || backup.count != 0 {
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

func TestWorldConfigurationRejectsExternalEditAndRereadsRuntimeFiles(t *testing.T) {
	service, roomPath, _ := newConfigurationService(t)
	current, err := service.WorldConfig("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	request := WorldUpdateRequest{ExpectedRevision: current.Revision, Server: current.Server}
	request.Server.ServerPort = 11000

	serverPath := filepath.Join(roomPath, "Master", "server.ini")
	serverData, err := os.ReadFile(serverPath)
	if err != nil {
		t.Fatal(err)
	}
	serverData = []byte(strings.Replace(string(serverData), "server_port = 10999", "server_port = 12001", 1))
	serverData = append(serverData, []byte("\n[OPERATOR]\nmanual_value = keep\n")...)
	if err := os.WriteFile(serverPath, serverData, 0o640); err != nil {
		t.Fatal(err)
	}
	luaPath := filepath.Join(roomPath, "Master", "leveldataoverride.lua")
	luaData, err := os.ReadFile(luaPath)
	if err != nil {
		t.Fatal(err)
	}
	luaData = []byte(strings.Replace(string(luaData), "    day = \"default\",", "    day = \"default\",\n    manual_external = { enabled = true },", 1))
	if err := os.WriteFile(luaPath, luaData, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := service.ApplyWorld(context.Background(), "job-stale-world", "room", "world", request); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale world revision error = %v", err)
	}
	writtenServer, _ := os.ReadFile(serverPath)
	writtenLua, _ := os.ReadFile(luaPath)
	if string(writtenServer) != string(serverData) || string(writtenLua) != string(luaData) {
		t.Fatalf("external world edit was overwritten:\nserver.ini:\n%s\nleveldataoverride.lua:\n%s", writtenServer, writtenLua)
	}
	latest, err := service.WorldConfig("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	manual, ok := latest.Overrides["manual_external"].(map[string]interface{})
	if latest.Revision == current.Revision || latest.Server.ServerPort != 12001 || latest.UnknownFieldCount != current.UnknownFieldCount+1 || !ok || manual["enabled"] != true {
		t.Fatalf("external world edit was not visible on reread: %#v", latest)
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
