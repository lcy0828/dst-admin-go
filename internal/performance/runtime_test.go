package performance

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

type placementSourceStub struct{ items []topology.ExecutionPlacement }

func (s placementSourceStub) ResolveCachedRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error) {
	return s.items, nil
}

type observationFunc func(context.Context, string, string, time.Time) error

func (f observationFunc) EnsureRuntimeInstallationFresh(ctx context.Context, target, installation string, after time.Time) error {
	return f(ctx, target, installation, after)
}

type worldReaderStub struct {
	calls []string
	state string
	err   error
	at    time.Time
}

type worldReaderFunc func(context.Context, string, string) (dstruntime.CurrentWorldState, error)

func (f worldReaderFunc) ReadCurrentWorldState(ctx context.Context, room, world string) (dstruntime.CurrentWorldState, error) {
	return f(ctx, room, world)
}

func TestScoringUsesProducerCaptureTimeWhenNativeCounterTracksActiveTime(t *testing.T) {
	observed := time.Now()
	captured := observed.UTC().Truncate(time.Second)
	sim, native := 100.0, 175.0
	src := RuntimeSource{
		Placements: placementSourceStub{[]topology.ExecutionPlacement{localPlacement("Master")}},
		Now:        func() time.Time { return observed },
		Reader: worldReaderFunc(func(context.Context, string, string) (dstruntime.CurrentWorldState, error) {
			return dstruntime.CurrentWorldState{Runtime: shared.ShardRuntimeStatus{State: "running"}, Snapshot: dstruntime.WorldStateSnapshot{
				ProducerInstanceID: "boot", SessionID: "session", CapturedAt: captured,
				SimulationSeconds: number(sim), RealSeconds: number(native), TimeScale: number(1),
			}}, nil
		}),
	}
	before, err := src.Samples(context.Background(), "room")
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	observed, captured = observed.Add(30*time.Second), captured.Add(30*time.Second)
	sim, native = sim+30, native+4
	after, err := src.Samples(context.Background(), "room")
	if err != nil || len(after) != 1 {
		t.Fatal(after, err)
	}
	p := score(&before[0], after[0], 30*time.Second)
	if p.Score == nil || *p.Score != 100 || p.ValidSeconds != 30 || p.Reason != "" {
		t.Fatalf("native active time rejected a healthy real-time simulation: %+v", p)
	}
	sim, native = sim+24, native+3
	observed, captured = observed.Add(30*time.Second), captured.Add(30*time.Second)
	slower, err := src.Samples(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	p = score(&after[0], slower[0], 30*time.Second)
	if p.Score == nil || *p.Score != 80 {
		t.Fatalf("real simulation slowdown was not measured: %+v", p)
	}
}

func (s *worldReaderStub) ReadCurrentWorldState(_ context.Context, _, world string) (dstruntime.CurrentWorldState, error) {
	s.calls = append(s.calls, world)
	return dstruntime.CurrentWorldState{Runtime: shared.ShardRuntimeStatus{State: s.state}, Snapshot: dstruntime.WorldStateSnapshot{ProducerInstanceID: "boot", SessionID: "session", CapturedAt: s.at, SimulationSeconds: number(100), RealSeconds: number(100), TimeScale: number(1)}}, s.err
}

func localPlacement(world string) topology.ExecutionPlacement {
	return topology.ExecutionPlacement{
		Room: rooms.Room{ID: "room", DirectoryName: "all"}, World: rooms.World{ID: world, Name: world, DirectoryName: world},
		AppliedTargetID: "local", AppliedInstallationID: "default",
		Target:    agents.RuntimeTarget{ID: "local", Online: true, Configured: true, Config: agents.RuntimeConfig{SavePath: "/data/saves", ServerPath: "/data/server"}},
		Inventory: agents.RuntimeTargetInventory{Stale: true},
	}
}

func TestLocalSamplingWorksWithNoBrowserOrInventoryAfterRestart(t *testing.T) {
	at := time.Now().UTC()
	reader := &worldReaderStub{state: "running", at: at}
	probes := 0
	src := RuntimeSource{
		Placements: placementSourceStub{[]topology.ExecutionPlacement{localPlacement("Master"), localPlacement("Caves")}}, Reader: reader,
		Remote: observationFunc(func(context.Context, string, string, time.Time) error {
			t.Fatal("local sampling requested an inventory refresh")
			return nil
		}),
		LocalStatus: func(context.Context, string, string) (shared.ShardRuntimeStatus, error) {
			return shared.ShardRuntimeStatus{State: reader.state}, nil
		},
		LocalProcesses: func(context.Context) ([]shared.ShardProcessReport, error) {
			probes++
			return []shared.ShardProcessReport{
				{PID: 99, Cluster: "all", Shard: "Master", StorageRoot: "/another-installation", RSSBytes: 999},
				{PID: 1, Cluster: "all", Shard: "Master", StorageRoot: "/data", ConfigDirectory: "saves", CPUSeconds: number(3), SampledAt: at, StartedAt: &at, RSSBytes: 100},
				{PID: 2, Cluster: "all", Shard: "Caves", StorageRoot: "/data/saves", CPUSeconds: number(5), SampledAt: at, StartedAt: &at, RSSBytes: 200},
			}, nil
		},
	}
	values, err := src.Samples(context.Background(), "room")
	if err != nil || len(values) != 2 || len(reader.calls) != 2 || probes != 1 {
		t.Fatalf("cold-start sampling depended on page observations: %+v, calls=%v probes=%d err=%v", values, reader.calls, probes, err)
	}
	for i, v := range values {
		if v.State != "running" || v.InstanceID == "" || v.SimulationSeconds == nil || v.ProcessID != int32(i+1) || v.RSSBytes != uint64((i+1)*100) || v.CPUSeconds == nil {
			t.Fatalf("world %d lost current clocks/resources or used another installation: %+v", i, v)
		}
	}
	reader.state, reader.err = "stopped", dstruntime.ErrWorldStateAbsent
	values, err = src.Samples(context.Background(), "room")
	if err != nil || probes != 1 || len(reader.calls) != 2 || len(values) != 2 || values[0].State != "stopped" || values[1].State != "stopped" {
		t.Fatalf("stopped room should not probe processes or invent an outage: %+v probes=%d err=%v", values, probes, err)
	}
}

func TestRemoteSamplingNeverFallsBackToLocalProcesses(t *testing.T) {
	for _, online := range []bool{false, true} {
		p := localPlacement("Caves")
		p.AppliedTargetID, p.Target.ID, p.Target.Online = "agent:node", "agent:node", online
		reader := &worldReaderStub{state: "running", at: time.Now().UTC()}
		src := RuntimeSource{Placements: placementSourceStub{[]topology.ExecutionPlacement{p}}, Reader: reader, LocalProcesses: func(context.Context) ([]shared.ShardProcessReport, error) {
			t.Fatal("remote world fell back to local process counters")
			return nil, nil
		}}
		values, err := src.Samples(context.Background(), "room")
		if err != nil || len(values) != 1 || values[0].State != "unavailable" || len(reader.calls) != 0 {
			t.Fatalf("stale/offline Agent was read as local: %+v %v", values, err)
		}
	}
}

func TestRemoteSamplingRefreshesOnlyTheAppliedInstallationOnceWithoutBrowser(t *testing.T) {
	at := time.Now().UTC()
	placements := []topology.ExecutionPlacement{localPlacement("Master"), localPlacement("Caves")}
	for i := range placements {
		placements[i].AppliedTargetID, placements[i].Target.ID = "agent:node", "agent:node"
	}
	reader := &worldReaderStub{state: "running", at: at}
	refreshes := 0
	src := RuntimeSource{Placements: placementSourceStub{placements}, Reader: reader, Remote: observationFunc(func(_ context.Context, target, installation string, after time.Time) error {
		refreshes++
		if target != "agent:node" || installation != "default" || after.IsZero() {
			t.Fatalf("unexpected refresh scope: %s/%s at %v", target, installation, after)
		}
		for i := range placements {
			placements[i].Inventory.Available, placements[i].Inventory.Stale = true, false
			placements[i].Inventory.Inventory.Processes = []shared.ShardProcessReport{{Cluster: "all", Shard: placements[i].World.DirectoryName, PID: int32(i + 10), RSSBytes: 500, CPUSeconds: number(1), StartedAt: &at, SampledAt: at}}
		}
		return nil
	})}
	values, err := src.Samples(context.Background(), "room")
	if err != nil || refreshes != 1 || len(reader.calls) != 2 || len(values) != 2 || values[0].TargetID != "agent:node" || values[0].RSSBytes != 500 || values[1].State != "running" {
		t.Fatalf("remote cold-start sampling failed or repeated installation refresh: %+v refreshes=%d err=%v", values, refreshes, err)
	}
	src.Remote = observationFunc(func(context.Context, string, string, time.Time) error { return errors.New("offline") })
	reader.calls = nil
	values, err = src.Samples(context.Background(), "room")
	if err != nil || len(reader.calls) != 0 || values[0].State != "unavailable" || values[0].CPUSeconds != nil {
		t.Fatalf("failed refresh reused old resource counters: %+v %v", values, err)
	}
}
