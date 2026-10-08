package dstruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/runtimefiles"
	"dont/shared"
)

type currentWorldFilesFixture struct {
	value      shared.RuntimeWorldStateRead
	err        error
	calls      int
	otherCalls int
}

func (f *currentWorldFilesFixture) ReadWorldState(ctx context.Context, _, _ string) (shared.RuntimeWorldStateRead, error) {
	f.calls++
	if ctx.Err() != nil {
		return shared.RuntimeWorldStateRead{}, ctx.Err()
	}
	return f.value, f.err
}

func (f *currentWorldFilesFixture) Status(context.Context, string, string) (shared.ShardRuntimeStatus, error) {
	f.otherCalls++
	return shared.ShardRuntimeStatus{}, errors.New("unexpected separate status read")
}

func (f *currentWorldFilesFixture) ReadArtifacts(context.Context, string, string, shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	f.otherCalls++
	return shared.RuntimeArtifactBundle{}, errors.New("unexpected separate artifact read")
}

func (f *currentWorldFilesFixture) SendID(context.Context, string, string, shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error) {
	f.otherCalls++
	return shared.RuntimeOperationResult{}, errors.New("unexpected console input")
}

func TestCurrentWorldStateReadsLatestCompleteFileOnceWithoutHealthOrRefresh(t *testing.T) {
	bridge, _, roomID, worldID, now := newDistributedBridgeFixture(t)
	snapshot := WorldStateSnapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: "2.4.5", ProducerInstanceID: "current", SessionID: "SESSION", ShardID: "1",
		Sequence: 3, CapturedAtUnix: now.Add(-time.Hour).Unix(), Complete: true, Season: "winter", Phase: "night",
	}
	bundle := artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-a.json", snapshot)
	snapshot.Sequence++
	snapshot.CapturedAtUnix++
	second := artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-b.json", snapshot)
	bundle.Artifacts = append(bundle.Artifacts, second.Artifacts...)
	fixture := &currentWorldFilesFixture{value: shared.RuntimeWorldStateRead{
		Runtime: shared.ShardRuntimeStatus{State: "running"}, StartedAt: now.Add(-2 * time.Hour),
		SessionID: "SESSION", ShardID: "1", Artifacts: bundle,
	}}
	bridge.runtime = fixture
	value, err := bridge.ReadCurrentWorldState(context.Background(), roomID, worldID)
	if err != nil || value.Snapshot.Sequence != 4 || !value.Snapshot.CapturedAt.Equal(time.Unix(snapshot.CapturedAtUnix, 0)) || fixture.calls != 1 || fixture.otherCalls != 0 {
		t.Fatalf("current read=%#v, error=%v, calls=%d/%d", value, err, fixture.calls, fixture.otherCalls)
	}
	fixture.value.Artifacts.Artifacts[1] = artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-b.json", map[string]string{"broken": "payload"}).Artifacts[0]
	value, err = bridge.ReadCurrentWorldState(context.Background(), roomID, worldID)
	if err != nil || value.Snapshot.Sequence != 3 {
		t.Fatalf("did not use other complete slot: %#v, %v", value, err)
	}
	fixture.value.Runtime.State = "stopped"
	if _, err := bridge.ReadCurrentWorldState(context.Background(), roomID, worldID); err != nil {
		t.Fatalf("stopped file failed: %v", err)
	}
}

func TestCurrentWorldStateRejectsPreviousBootAndWrongIdentity(t *testing.T) {
	bridge, _, roomID, worldID, now := newDistributedBridgeFixture(t)
	snapshot := WorldStateSnapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "previous", SessionID: "SESSION", ShardID: "1",
		Sequence: 200, CapturedAtUnix: now.Add(-time.Minute).Unix(), Complete: true, Season: "winter",
	}
	fixture := &currentWorldFilesFixture{value: shared.RuntimeWorldStateRead{
		Runtime: shared.ShardRuntimeStatus{State: "running"}, StartedAt: now.Add(-time.Second), SessionID: "SESSION", ShardID: "1",
		Artifacts: artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-a.json", snapshot),
	}}
	bridge.runtime = fixture
	if _, err := bridge.ReadCurrentWorldState(context.Background(), roomID, worldID); !errors.Is(err, ErrWorldStatePending) {
		t.Fatalf("previous boot accepted: %v", err)
	}
	fixture.value.StartedAt = now.Add(-time.Hour)
	for _, change := range []func(*WorldStateSnapshot){
		func(s *WorldStateSnapshot) { s.SessionID = "OTHER" },
		func(s *WorldStateSnapshot) { s.ShardID = "2" },
		func(s *WorldStateSnapshot) { s.Complete = false },
		func(s *WorldStateSnapshot) { s.CapturedAtUnix = now.Add(time.Hour).Unix() },
	} {
		invalid := snapshot
		change(&invalid)
		fixture.value.Artifacts = artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-a.json", invalid)
		if _, err := bridge.ReadCurrentWorldState(context.Background(), roomID, worldID); err == nil || errors.Is(err, ErrWorldStatePending) {
			t.Fatalf("invalid file error=%v", err)
		}
	}
	fixture.value.ReadError = "permission denied"
	value, err := bridge.ReadCurrentWorldState(context.Background(), roomID, worldID)
	if !errors.Is(err, ErrSnapshotUnavailable) || value.Runtime.State != "running" {
		t.Fatalf("read failure=%#v, %v", value, err)
	}
	fixture.err = context.DeadlineExceeded
	if _, err := bridge.ReadCurrentWorldState(context.Background(), roomID, worldID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}
}

func TestCurrentWorldStateUsesEffectiveShardIdentityFromRuntimeFiles(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		cluster    string
		configured string
		sampled    string
		wantErr    error
	}{
		{name: "unsharded master configured as one", cluster: "[SHARD]\nshard_enabled = false\n", configured: "1", sampled: "0"},
		{name: "unsharded master configured as zero", cluster: "[SHARD]\nshard_enabled = false\n", configured: "0", sampled: "0"},
		{name: "unsharded rejects foreign shard", cluster: "[SHARD]\nshard_enabled = false\n", configured: "1", sampled: "1", wantErr: ErrSnapshotStale},
		{name: "sharded preserves custom master ID", cluster: "[SHARD]\nshard_enabled = true\n", configured: "7", sampled: "7"},
		{name: "sharded rejects zero sample", cluster: "[SHARD]\nshard_enabled = true\n", configured: "1", sampled: "0", wantErr: ErrSnapshotStale},
		{name: "legacy config retains explicit ID", configured: "1", sampled: "1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			bridge, _, roomID, worldID, now := newDistributedBridgeFixture(t)
			root := t.TempDir()
			world := filepath.Join(root, "Cluster", "Master")
			for _, name := range []string{"save/session/SESSION", "save/mod_config_data/dst-admin"} {
				if err := os.MkdirAll(filepath.Join(world, name), 0750); err != nil {
					t.Fatal(err)
				}
			}
			server := "[SHARD]\nis_master = true\nid = " + testCase.configured + "\n"
			for path, data := range map[string]string{
				filepath.Join(root, "Cluster", "cluster.ini"): testCase.cluster,
				filepath.Join(world, "server.ini"):            server,
				filepath.Join(world, "server_log.txt"):        "[00:00:00]: Current time: " + now.Add(-time.Minute).In(time.Local).Format(time.ANSIC) + "\n",
			} {
				if err := os.WriteFile(path, []byte(data), 0640); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := WorldStateSnapshot{
				SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "current", SessionID: "SESSION", ShardID: testCase.sampled,
				Sequence: 1, CapturedAtUnix: now.Add(-10 * time.Second).Unix(), Complete: true, Season: "autumn",
			}
			for _, slot := range []string{"worldstate-a.json", "worldstate-b.json"} {
				writeSnapshot(t, filepath.Join(world, "save/mod_config_data/dst-admin", slot), snapshot)
				snapshot.Sequence++
			}
			files, err := runtimefiles.ReadWorldState(context.Background(), root, "Cluster", "Master", shared.ShardRuntimeStatus{State: "running"})
			if err != nil || files.ReadError != "" {
				t.Fatalf("runtime file read=%#v, %v", files, err)
			}
			fixture := &currentWorldFilesFixture{value: files}
			bridge.runtime = fixture
			value, err := bridge.ReadCurrentWorldState(context.Background(), roomID, worldID)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("state read error=%v, want %v", err, testCase.wantErr)
			}
			if err == nil && (value.Snapshot.ShardID != testCase.sampled || value.Snapshot.Sequence != 2) {
				t.Fatalf("wrong snapshot=%#v", value)
			}
			if fixture.calls != 1 || fixture.otherCalls != 0 {
				t.Fatalf("state read sent extra requests: %d/%d", fixture.calls, fixture.otherCalls)
			}
			written, err := os.ReadFile(filepath.Join(world, "server.ini"))
			if err != nil || string(written) != server {
				t.Fatalf("state read rewrote server.ini: %v", err)
			}
		})
	}
}

func TestCurrentWorldStateDistinguishesFirstSampleFromReadFailure(t *testing.T) {
	bridge, _, roomID, worldID, now := newDistributedBridgeFixture(t)
	fixture := &currentWorldFilesFixture{value: shared.RuntimeWorldStateRead{
		StartedAt: now.Add(-time.Second), SessionID: "SESSION", ShardID: "1",
		Artifacts: shared.RuntimeArtifactBundle{Kind: shared.ArtifactRuntimeWorldState},
	}}
	bridge.runtime = fixture
	for _, state := range []string{"starting", "running", "stopped"} {
		fixture.value.Runtime.State = state
		_, err := bridge.ReadCurrentWorldState(context.Background(), roomID, worldID)
		if state == "stopped" {
			if !errors.Is(err, ErrWorldStateAbsent) {
				t.Fatalf("stopped world cannot produce a first sample: %v", err)
			}
		} else if !errors.Is(err, ErrWorldStatePending) {
			t.Fatalf("state=%s missing first sample: %v", state, err)
		}
		fixture.value.ReadError = "permission denied"
		_, err = bridge.ReadCurrentWorldState(context.Background(), roomID, worldID)
		if !errors.Is(err, ErrSnapshotUnavailable) || errors.Is(err, ErrWorldStatePending) {
			t.Fatalf("state=%s hid read failure: %v", state, err)
		}
		fixture.value.ReadError = ""
	}
}

func TestPausedHealthRequiresCurrentProcessIdentity(t *testing.T) {
	bridge, _, roomID, worldID, now := newDistributedBridgeFixture(t)
	paused := true
	captured := now.Add(-time.Minute).Unix()
	health := Health{
		ProducerInstanceID: "telemetry", SessionID: "SESSION", ShardID: "1",
		Running: true, Ready: true, ReadAt: now.Add(-time.Minute),
		LastCapturedAtUnix: &captured, LastWrittenAtUnix: &captured,
	}
	current := shared.RuntimeWorldStateRead{
		Runtime:   shared.ShardRuntimeStatus{State: "running", Paused: &paused},
		StartedAt: now.Add(-time.Hour), SessionID: "SESSION", ShardID: "1",
	}
	for _, test := range []struct {
		name   string
		change func(*shared.RuntimeWorldStateRead, *Health)
		want   bool
	}{
		{name: "same boot accepts paused sample", want: true},
		{name: "restart of same save rejects old health", change: func(v *shared.RuntimeWorldStateRead, h *Health) {
			v.StartedAt = now.Add(-10 * time.Second)
			h.ReadAt = now // A copied file's new mtime is not proof of this boot.
		}},
		{name: "missing startup identity", change: func(v *shared.RuntimeWorldStateRead, _ *Health) { v.StartedAt = time.Time{} }},
		{name: "different save", change: func(v *shared.RuntimeWorldStateRead, _ *Health) { v.SessionID = "OTHER" }},
		{name: "different shard", change: func(v *shared.RuntimeWorldStateRead, _ *Health) { v.ShardID = "2" }},
		{name: "missing capture time", change: func(_ *shared.RuntimeWorldStateRead, h *Health) { h.LastCapturedAtUnix = nil }},
		{name: "future capture", change: func(_ *shared.RuntimeWorldStateRead, h *Health) {
			future := now.Add(time.Hour).Unix()
			h.LastCapturedAtUnix, h.LastWrittenAtUnix = &future, &future
		}},
		{name: "process stopped during read", change: func(v *shared.RuntimeWorldStateRead, _ *Health) { v.Runtime.State = "stopped" }},
		{name: "process resumed during read", change: func(v *shared.RuntimeWorldStateRead, _ *Health) {
			unpaused := false
			v.Runtime.Paused = &unpaused
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, sample := current, health
			if test.change != nil {
				test.change(&value, &sample)
			}
			fixture := &currentWorldFilesFixture{value: value}
			bridge.runtime = fixture
			got, err := bridge.HealthMatchesCurrentProcess(context.Background(), roomID, worldID, sample)
			if err != nil || got != test.want || fixture.calls != 1 || fixture.otherCalls != 0 {
				t.Fatalf("current=%v, error=%v, reads=%d, other calls=%d", got, err, fixture.calls, fixture.otherCalls)
			}
		})
	}
	fixture := &currentWorldFilesFixture{value: current, err: context.DeadlineExceeded}
	bridge.runtime = fixture
	if got, err := bridge.HealthMatchesCurrentProcess(context.Background(), roomID, worldID, health); got || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read failure accepted: %v, %v", got, err)
	}
	fixture.err = nil
	fixture.value.ReadError = "permission denied"
	if got, err := bridge.HealthMatchesCurrentProcess(context.Background(), roomID, worldID, health); got || !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("file failure accepted: %v, %v", got, err)
	}
}
