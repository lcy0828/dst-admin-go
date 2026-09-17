package configuration

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"

	"dont/internal/rooms"
	"dont/internal/runtimedriver"
)

// verifyWorldConfiguration rereads the files from the world Runtime after an
// atomic publication. A successful publication is not reported as complete
// until the disk revision can be observed through the same Runtime contract.
func (s *Service) verifyWorldConfiguration(ctx context.Context, roomID, worldID, expectedRevision string) (SyncState, error) {
	snapshot, err := s.readRuntimeConfiguration(ctx, roomID, worldID, string(PublicationWorld))
	if err != nil {
		return SyncState{}, fmt.Errorf("发布后回读世界配置: %w", err)
	}
	server, serverModified, err := configurationSnapshotFile(snapshot.Result.Files, "server.ini", false)
	if err != nil {
		return SyncState{}, err
	}
	override, overrideModified, err := configurationSnapshotFile(snapshot.Result.Files, "leveldataoverride.lua", false)
	if err != nil {
		return SyncState{}, err
	}
	document, err := parseWorldDocument(server, serverModified, override, overrideModified)
	if err != nil {
		return SyncState{}, err
	}
	if document.revision != expectedRevision {
		return SyncState{}, fmt.Errorf("发布后世界配置 revision 不一致: 期望 %s，实际 %s", expectedRevision, document.revision)
	}
	return s.observeRuntimeConfiguration(roomID, worldID, string(PublicationWorld), snapshot, document.revision, document.modified), nil
}

// verifyRoomConfiguration confirms the user-managed fields on every Runtime
// target. Topology-owned SHARD addresses are rendered independently per target
// and are intentionally excluded from this comparison.
func (s *Service) verifyRoomConfiguration(ctx context.Context, roomID string, expected RoomValues) (SyncState, error) {
	catalog, ok := s.rooms.(roomWorldCatalog)
	if !ok {
		return SyncState{}, fmt.Errorf("房间目录无法枚举世界")
	}
	worlds, err := catalog.Worlds(roomID)
	if err != nil {
		return SyncState{}, err
	}
	if len(worlds) == 0 {
		return SyncState{}, rooms.ErrWorldNotFound
	}
	sort.SliceStable(worlds, func(i, j int) bool { return worlds[i].IsMaster && !worlds[j].IsMaster })
	seenTargets := make(map[string]bool, len(worlds))
	var observed runtimedriver.ConfigurationSnapshot
	var observedDocument roomDocument
	for _, world := range worlds {
		snapshot, readErr := s.readRuntimeConfiguration(ctx, roomID, world.ID, string(PublicationShared))
		if readErr != nil {
			return SyncState{}, fmt.Errorf("回读 %s: %w", world.Name, readErr)
		}
		key := snapshot.Target.TargetID + "\x00" + snapshot.Target.InstallationID
		if seenTargets[key] {
			continue
		}
		seenTargets[key] = true
		file, modified, fileErr := configurationSnapshotFile(snapshot.Result.Files, "cluster.ini", false)
		if fileErr != nil {
			return SyncState{}, fmt.Errorf("回读 %s: %w", world.Name, fileErr)
		}
		document, parseErr := parseRoomDocument(file.data, file.mode, modified, file.exists)
		if parseErr != nil {
			return SyncState{}, fmt.Errorf("解析 %s: %w", world.Name, parseErr)
		}
		if !sameManagedRoomValues(document.values, expected) {
			return SyncState{}, fmt.Errorf("%s 的 cluster.ini 与刚保存的配置不一致", world.Name)
		}
		if observed.Result.Files == nil || world.IsMaster {
			observed, observedDocument = snapshot, document
		}
	}
	if len(observed.Result.Files) == 0 {
		return SyncState{}, os.ErrNotExist
	}
	return s.observeRuntimeConfiguration(roomID, "", string(PublicationShared), observed, observedDocument.revision, observedDocument.modified), nil
}

func sameManagedRoomValues(actual, expected RoomValues) bool {
	actual.BindIP, expected.BindIP = "", ""
	actual.MasterIP, expected.MasterIP = "", ""
	actual.MasterPort, expected.MasterPort = 0, 0
	return reflect.DeepEqual(actual, expected)
}

func roomPublicationFiles(data []byte, mode os.FileMode) []rooms.ProvisionFile {
	return []rooms.ProvisionFile{{Name: "cluster.ini", Data: append([]byte(nil), data...), Mode: mode}}
}

func worldPublicationFiles(serverData []byte, serverMode os.FileMode, luaData []byte, luaMode os.FileMode) []rooms.ProvisionFile {
	return []rooms.ProvisionFile{
		{Name: "server.ini", Data: append([]byte(nil), serverData...), Mode: serverMode},
		{Name: "leveldataoverride.lua", Data: append([]byte(nil), luaData...), Mode: luaMode},
	}
}
