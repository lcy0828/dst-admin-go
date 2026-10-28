package performance

import (
	"context"
	"errors"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/runtimeinventory"
	"dont/internal/topology"
	"dont/shared"
)

type Placements interface {
	ResolveCachedRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error)
}
type RuntimeReader interface {
	ReadCurrentWorldState(context.Context, string, string) (dstruntime.CurrentWorldState, error)
}
type RemoteObservations interface {
	EnsureRuntimeInstallationFresh(context.Context, string, string, time.Time) error
}
type RuntimeSource struct {
	Placements     Placements
	Reader         RuntimeReader
	LocalProcesses func(context.Context) ([]shared.ShardProcessReport, error)
	LocalStatus    func(context.Context, string, string) (shared.ShardRuntimeStatus, error)
	Remote         RemoteObservations
	Now            func() time.Time
}

// Remote targets share a coalesced observation of only their applied installation.
// Local sampling reads
// the selected world's current Runtime and probes process counters once per
// active room batch; it never depends on a browser's inventory lease. No game
// console commands, local filesystem inventories or global refreshes are requested.
func (s RuntimeSource) Samples(ctx context.Context, room string) ([]Input, error) {
	if s.Placements == nil || s.Reader == nil {
		return nil, errors.New("performance runtime source unavailable")
	}
	placements, err := s.Placements.ResolveCachedRoomExecutions(ctx, room)
	if err != nil {
		return nil, err
	}
	failed := map[string]bool{}
	if s.Remote != nil {
		seen := map[string]bool{}
		refresh := false
		after := time.Now().UTC().Add(-15 * time.Second)
		for _, p := range placements {
			key := p.AppliedTargetID + "/" + p.AppliedInstallationID
			if p.AppliedTargetID == "local" || !p.Target.Online || !p.Target.Configured || seen[key] {
				continue
			}
			seen[key] = true
			if err := s.Remote.EnsureRuntimeInstallationFresh(ctx, p.AppliedTargetID, p.AppliedInstallationID, after); err != nil {
				failed[key] = true
			} else {
				refresh = true
			}
		}
		if refresh {
			placements, err = s.Placements.ResolveCachedRoomExecutions(ctx, room)
			if err != nil {
				return nil, err
			}
		}
	}
	result := make([]Input, 0, len(placements))
	var localProcesses []shared.ShardProcessReport
	localRead := false
	for _, p := range placements {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		observed := time.Now()
		if s.Now != nil {
			observed = s.Now()
		}
		in := Input{RoomID: room, WorldID: p.World.ID, WorldName: p.World.Name, TargetID: p.AppliedTargetID, InstallationID: p.AppliedInstallationID, At: observed.UTC(), ObservedAt: observed, State: "stopped"}
		local := p.AppliedTargetID == "local"
		if !p.Target.Online || !p.Target.Configured || !local && (failed[p.AppliedTargetID+"/"+p.AppliedInstallationID] || !p.Inventory.Available || p.Inventory.Stale) {
			in.State = "unavailable"
			result = append(result, in)
			continue
		}
		if !local && !applyProcess(&in, p, p.Inventory.Inventory.Processes) {
			result = append(result, in)
			continue
		}
		if local && s.LocalStatus != nil {
			status, err := s.LocalStatus(ctx, room, p.World.ID)
			if err != nil {
				in.State = "unavailable"
				result = append(result, in)
				continue
			}
			in.State = status.State
			if status.State != "running" {
				result = append(result, in)
				continue
			}
		}
		value, err := s.Reader.ReadCurrentWorldState(ctx, room, p.World.ID)
		in.State = value.Runtime.State
		if err != nil {
			if in.State != "stopped" && in.State != "failed" {
				in.State = "unavailable"
			}
			result = append(result, in)
			continue
		}
		if local && in.State == "running" && s.LocalProcesses != nil {
			if !localRead {
				localRead = true
				if processes, err := s.LocalProcesses(ctx); err == nil {
					localProcesses = processes
				}
			}
			processes := runtimeinventory.ProcessesForInstallation(localProcesses, p.Target.Config.SavePath, p.Target.Config.ServerPath)
			applyProcess(&in, p, processes)
		}
		v := value.Snapshot
		in.InstanceID = p.AppliedTargetID + "/" + p.AppliedInstallationID + "/" + v.ProducerInstanceID + "/" + v.SessionID + "/" + ScoreVersion
		in.Paused = value.Runtime.Paused != nil && *value.Runtime.Paused || v.SimulationPaused != nil && *v.SimulationPaused
		if !in.Paused {
			in.At = v.CapturedAt
		}
		in.SimulationSeconds = v.SimulationSeconds
		// TheSim:GetRealTime() can count active execution rather than elapsed
		// wall time. Use the producer's os.time() capture timestamp instead;
		// the controller's monotonic observation guards wall-clock jumps.
		if !v.CapturedAt.IsZero() {
			in.CaptureSeconds = number(float64(v.CapturedAt.Unix()))
		}
		in.TimeScale = v.TimeScale
		in.NativePerformance = v.HostPerformance
		result = append(result, in)
	}
	return result, nil
}

func applyProcess(in *Input, p topology.ExecutionPlacement, processes []shared.ShardProcessReport) bool {
	for _, process := range processes {
		if process.Cluster != p.Room.DirectoryName || process.Shard != p.World.DirectoryName {
			continue
		}
		in.ProcessID = process.PID
		in.CPUSeconds = process.CPUSeconds
		in.CPUSampledAt = process.SampledAt
		in.RSSBytes = process.RSSBytes
		if process.StartedAt != nil {
			in.ProcessStartedAt = *process.StartedAt
		}
		return true
	}
	return false
}
