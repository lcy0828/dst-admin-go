package configuration

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/runtimedriver"
	"dont/shared"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newConfigurationStateStore(t *testing.T) *StateStore {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewStateStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	return store
}

func configurationStateSnapshot(data string) runtimedriver.ConfigurationSnapshot {
	return runtimedriver.ConfigurationSnapshot{
		Target: runtimedriver.Target{TargetID: "agent:test", InstallationID: "native"},
		Result: shared.RuntimeConfigurationResult{Complete: true, Files: []shared.RuntimeConfigurationFile{{
			Name: "cluster.ini", Exists: true, Mode: 0o640, Size: int64(len(data)), Data: []byte(data),
		}}},
	}
}

func TestConfigurationStateStorePersistsOnlyRuntimeObservation(t *testing.T) {
	store := newConfigurationStateStore(t)
	observedAt := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	state, err := store.Observe("room", "", "shared", configurationStateSnapshot("one"), "rev-one", observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if state.ObservedRevision != "rev-one" {
		t.Fatalf("initial state = %#v", state)
	}
	observed, ok := state.ObservedSnapshot()
	if !ok || string(observed.Result.Files[0].Data) != "one" {
		t.Fatalf("observed snapshot = %#v, %v", observed, ok)
	}

	state, err = store.Observe("room", "", "shared", configurationStateSnapshot("two"), "rev-two", observedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if state.ObservedRevision != "rev-two" {
		t.Fatalf("updated state = %#v", state)
	}
	record, found, err := store.record("room", "", "shared")
	if err != nil || !found || record.DesiredFiles != "" || record.DesiredRevision != "" {
		t.Fatalf("legacy desired state was not cleared: %#v, %v", record, err)
	}
}

type switchableConfigurationReader struct {
	snapshot runtimedriver.ConfigurationSnapshot
	err      error
}

func (r *switchableConfigurationReader) ReadConfiguration(context.Context, string, string, string) (runtimedriver.ConfigurationSnapshot, error) {
	if r.err != nil {
		return runtimedriver.ConfigurationSnapshot{}, r.err
	}
	return r.snapshot, nil
}

func TestRoomConfigurationFallsBackToPersistedSnapshot(t *testing.T) {
	service, _, _ := newConfigurationService(t)
	store := newConfigurationStateStore(t)
	if err := service.ConfigureStateRepository(store); err != nil {
		t.Fatal(err)
	}
	reader := &switchableConfigurationReader{snapshot: configurationStateSnapshot("[NETWORK]\ncluster_name = Cached\ncluster_language = zh\n[GAMEPLAY]\ngame_mode = survival\nmax_players = 6\n")}
	if err := service.ConfigureReader(reader); err != nil {
		t.Fatal(err)
	}
	current, err := service.RoomConfig("room")
	if err != nil || current.Values.ClusterName != "Cached" || current.Sync.Status != "synced" {
		t.Fatalf("current = %#v, %v", current, err)
	}

	reader.err = errors.New("agent timed out")
	cached, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	if cached.Values.ClusterName != "Cached" || !cached.Sync.Stale || cached.Sync.Status != "stale" || cached.Sync.LastError == "" {
		t.Fatalf("cached = %#v", cached)
	}
}

func TestRoomPreviewAndApplyNeverUsePersistedSnapshot(t *testing.T) {
	service, _, _ := newConfigurationService(t)
	store := newConfigurationStateStore(t)
	if err := service.ConfigureStateRepository(store); err != nil {
		t.Fatal(err)
	}
	reader := &switchableConfigurationReader{snapshot: configurationStateSnapshot("[NETWORK]\ncluster_name = Runtime\ncluster_language = zh\n[GAMEPLAY]\ngame_mode = survival\nmax_players = 6\n")}
	if err := service.ConfigureReader(reader); err != nil {
		t.Fatal(err)
	}
	current, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	request := RoomUpdateRequest{ExpectedRevision: current.Revision, Values: current.Values}
	request.Values.ClusterDescription = "must not use stale data"
	offlineErr := errors.New("runtime is offline")
	reader.err = offlineErr
	if _, err := service.PreviewRoom("room", request); !errors.Is(err, offlineErr) {
		t.Fatalf("preview error=%v", err)
	}
	if _, err := service.ApplyRoom(context.Background(), "job", "room", request); !errors.Is(err, offlineErr) {
		t.Fatalf("apply error=%v", err)
	}
}
