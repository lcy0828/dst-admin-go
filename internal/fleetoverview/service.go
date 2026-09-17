package fleetoverview

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/rooms"
	"dont/internal/savehealth"
	"dont/internal/topology"
	"dont/shared"
)

var ErrRuntimeTargetNotFound = errors.New("runtime target not found")

type Topology interface {
	FleetTopology(context.Context) (topology.FleetSnapshot, error)
}

type Rooms interface {
	List() ([]rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type Runtime interface {
	Status(context.Context, string, string) (shared.ShardRuntimeStatus, error)
}

type Scope struct {
	Kind     string `json:"kind"`
	TargetID string `json:"targetId,omitempty"`
}

type Summary struct {
	Targets       int `json:"targets"`
	OnlineTargets int `json:"onlineTargets"`
	Rooms         int `json:"rooms"`
	MixedRooms    int `json:"mixedRooms"`
	Worlds        int `json:"worlds"`
	Running       int `json:"running"`
	Stopped       int `json:"stopped"`
	Attention     int `json:"attention"`
}

type Problem struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	TargetID string `json:"targetId,omitempty"`
	RoomID   string `json:"roomId,omitempty"`
	WorldID  string `json:"worldId,omitempty"`
	Severity string `json:"severity"`
}

type World struct {
	rooms.World
	Status           string                  `json:"status"`
	StatusCode       string                  `json:"statusCode,omitempty"`
	Paused           *bool                   `json:"paused,omitempty"`
	ControlAvailable bool                    `json:"controlAvailable"`
	StatusMessage    string                  `json:"statusMessage,omitempty"`
	Placement        topology.Placement      `json:"placement"`
	Target           *topology.TargetSummary `json:"target,omitempty"`
}

type Room struct {
	rooms.Room
	Worlds           []World  `json:"worlds"`
	TargetIDs        []string `json:"targetIds"`
	MixedPlacement   bool     `json:"mixedPlacement"`
	TopologyRevision string   `json:"topologyRevision"`
}

type Snapshot struct {
	Scope      Scope                    `json:"scope"`
	Summary    Summary                  `json:"summary"`
	Targets    []topology.TargetSummary `json:"targets"`
	Rooms      []Room                   `json:"rooms"`
	Issues     []Problem                `json:"issues"`
	ObservedAt time.Time                `json:"observedAt"`
}

type Service struct {
	topology Topology
	rooms    Rooms
	runtime  Runtime
	timeout  time.Duration
}

func New(topologyService Topology, roomService Rooms, runtime Runtime) (*Service, error) {
	if topologyService == nil || roomService == nil || runtime == nil {
		return nil, errors.New("fleet overview dependencies are required")
	}
	return &Service{topology: topologyService, rooms: roomService, runtime: runtime, timeout: 5 * time.Second}, nil
}

func (s *Service) Snapshot(ctx context.Context, targetID string) (Snapshot, error) {
	return s.snapshot(ctx, targetID, true)
}

// InventorySnapshot uses the collected process inventory without probing every
// world's logs and runtime health. Callers refresh inventories before this read.
func (s *Service) InventorySnapshot(ctx context.Context, targetID string) (Snapshot, error) {
	return s.snapshot(ctx, targetID, false)
}

func (s *Service) snapshot(ctx context.Context, targetID string, observeRuntime bool) (Snapshot, error) {
	targetID = strings.TrimSpace(targetID)
	fleet, err := s.topology.FleetTopology(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	targets := make(map[string]topology.TargetSummary, len(fleet.Targets))
	result := Snapshot{
		Scope: Scope{Kind: "all"}, Targets: append([]topology.TargetSummary(nil), fleet.Targets...),
		Rooms: []Room{}, Issues: []Problem{}, ObservedAt: fleet.ObservedAt,
	}
	for _, target := range fleet.Targets {
		targets[target.ID] = target
	}
	if targetID != "" {
		if _, exists := targets[targetID]; !exists {
			return Snapshot{}, ErrRuntimeTargetNotFound
		}
		result.Scope = Scope{Kind: "target", TargetID: targetID}
		result.Targets = []topology.TargetSummary{targets[targetID]}
	}
	result.Summary.Targets = len(result.Targets)
	for _, target := range result.Targets {
		if target.Online {
			result.Summary.OnlineTargets++
		}
	}
	seenProblems := map[string]struct{}{}
	appendProblem := func(problem Problem) {
		key := strings.Join([]string{problem.Code, problem.TargetID, problem.RoomID, problem.WorldID}, "\x00")
		if _, exists := seenProblems[key]; exists {
			return
		}
		seenProblems[key] = struct{}{}
		result.Issues = append(result.Issues, problem)
	}
	roomsByID, err := s.roomCatalog()
	if err != nil {
		return Snapshot{}, err
	}
	seenRooms := make(map[string]bool, len(fleet.Rooms))
	for _, roomTopology := range fleet.Rooms {
		room, exists := roomsByID[roomTopology.RoomID]
		if !exists {
			continue
		}
		seenRooms[room.ID] = true
		worlds, worldsErr := s.rooms.Worlds(room.ID)
		if worldsErr != nil {
			appendProblem(Problem{
				Code: "ROOM_WORLDS_UNAVAILABLE", Message: worldsErr.Error(), RoomID: room.ID, Severity: "error",
			})
			continue
		}
		worldsByID := make(map[string]rooms.World, len(worlds))
		for _, world := range worlds {
			worldsByID[world.ID] = world
		}
		item := Room{Room: room, Worlds: []World{}, TopologyRevision: roomTopology.Revision}
		allTargetIDs := make(map[string]bool)
		for _, placement := range roomTopology.Placements {
			appliedTargetID := strings.TrimSpace(placement.AppliedTargetID)
			if appliedTargetID != "" {
				allTargetIDs[appliedTargetID] = true
			}
			if targetID != "" && appliedTargetID != targetID {
				continue
			}
			world, worldExists := worldsByID[placement.WorldID]
			if !worldExists {
				continue
			}
			status, available := placementRuntimeStatus(placement, targets[appliedTargetID])
			target := targets[appliedTargetID]
			var targetPointer *topology.TargetSummary
			if target.ID != "" {
				value := target
				targetPointer = &value
			}
			item.Worlds = append(item.Worlds, World{
				World: world, Status: status, ControlAvailable: available,
				StatusMessage: placementMessage(placement, target), Placement: placement, Target: targetPointer,
			})
		}
		if len(item.Worlds) == 0 {
			continue
		}
		for value := range allTargetIDs {
			item.TargetIDs = append(item.TargetIDs, value)
		}
		sort.Strings(item.TargetIDs)
		item.MixedPlacement = len(item.TargetIDs) > 1
		result.Rooms = append(result.Rooms, item)
		for _, issue := range roomTopology.Issues {
			if targetID != "" && issue.TargetID != "" && issue.TargetID != targetID {
				continue
			}
			appendProblem(Problem{
				Code: issue.Code, Message: issue.Message, TargetID: issue.TargetID,
				RoomID: room.ID, WorldID: issue.WorldID, Severity: string(issue.Severity),
			})
		}
	}
	for roomID, room := range roomsByID {
		if seenRooms[roomID] {
			continue
		}
		worlds, worldsErr := s.rooms.Worlds(roomID)
		if worldsErr != nil {
			appendProblem(Problem{
				Code: "ROOM_WORLDS_UNAVAILABLE", Message: worldsErr.Error(), RoomID: roomID, Severity: "error",
			})
			continue
		}
		room.Managed = true
		room.ControlState = "unavailable"
		room.ControlAvailable = false
		item := Room{Room: room, Worlds: []World{}, TargetIDs: append([]string(nil), room.TargetIDs...)}
		for _, world := range worlds {
			if targetID != "" && !containsID(world.TargetIDs, targetID) {
				continue
			}
			worldTargetID := discoveredWorldTarget(world, targetID)
			target := targets[worldTargetID]
			var targetPointer *topology.TargetSummary
			if target.ID != "" {
				value := target
				targetPointer = &value
			}
			item.Worlds = append(item.Worlds, World{
				World: world, Status: "unknown", ControlAvailable: false,
				StatusMessage: "房间拓扑尚未就绪",
				Placement: topology.Placement{
					WorldID: world.ID, WorldName: world.Name, WorldRole: world.Role,
					DesiredTargetID: worldTargetID, AppliedTargetID: worldTargetID,
					State: topology.PlacementInventoryMissing,
				},
				Target: targetPointer,
			})
			item.TargetIDs = append(item.TargetIDs, world.TargetIDs...)
		}
		if len(item.Worlds) == 0 {
			continue
		}
		item.TargetIDs = uniqueIDs(item.TargetIDs)
		item.MixedPlacement = len(item.TargetIDs) > 1
		result.Rooms = append(result.Rooms, item)
		appendProblem(Problem{
			Code: "ROOM_TOPOLOGY_UNAVAILABLE", Message: "房间已被运行节点发现，但运行拓扑尚未就绪",
			TargetID: targetID, RoomID: roomID, Severity: "info",
		})
	}
	sort.SliceStable(result.Rooms, func(i, j int) bool {
		if !strings.EqualFold(result.Rooms[i].Name, result.Rooms[j].Name) {
			return strings.ToLower(result.Rooms[i].Name) < strings.ToLower(result.Rooms[j].Name)
		}
		return result.Rooms[i].ID < result.Rooms[j].ID
	})
	if observeRuntime {
		if err := s.observeRuntime(ctx, result.Rooms); err != nil {
			return Snapshot{}, err
		}
	}
	summarize(&result)
	return result, nil
}

func (s *Service) observeRuntime(ctx context.Context, roomItems []Room) error {
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for roomIndex := range roomItems {
		for worldIndex := range roomItems[roomIndex].Worlds {
			world := roomItems[roomIndex].Worlds[worldIndex]
			if !world.ControlAvailable {
				continue
			}
			roomIndex, worldIndex := roomIndex, worldIndex
			wait.Add(1)
			go func() {
				defer wait.Done()
				select {
				case semaphore <- struct{}{}:
					defer func() { <-semaphore }()
				case <-ctx.Done():
					return
				}
				statusContext, cancel := context.WithTimeout(ctx, s.timeout)
				defer cancel()
				current := roomItems[roomIndex].Worlds[worldIndex]
				status, err := s.runtime.Status(statusContext, current.RoomID, current.ID)
				if err != nil {
					current.Status = "unknown"
					current.ControlAvailable = false
					current.StatusMessage = err.Error()
					roomItems[roomIndex].Worlds[worldIndex] = current
					return
				}
				state := strings.ToLower(strings.TrimSpace(status.State))
				if state == "" {
					state = "unknown"
				}
				current.Status = state
				current.StatusCode = strings.ToUpper(strings.TrimSpace(status.Code))
				current.Paused = status.Paused
				if message := strings.TrimSpace(status.Message); message != "" {
					current.StatusMessage = message
				}
				roomItems[roomIndex].Worlds[worldIndex] = current
			}()
		}
	}
	wait.Wait()
	return ctx.Err()
}

func summarize(result *Snapshot) {
	result.Summary.Rooms = len(result.Rooms)
	result.Summary.MixedRooms = 0
	result.Summary.Worlds = 0
	result.Summary.Running = 0
	result.Summary.Stopped = 0
	result.Summary.Attention = 0
	for _, room := range result.Rooms {
		if room.MixedPlacement {
			result.Summary.MixedRooms++
		}
		for _, world := range room.Worlds {
			result.Summary.Worlds++
			switch world.Status {
			case "running":
				result.Summary.Running++
			case "stopped":
				result.Summary.Stopped++
			default:
				result.Summary.Attention++
			}
			if world.Status == "running" && world.StatusCode == savehealth.SaveWriteFailedCode {
				result.Summary.Attention++
			}
		}
	}
}

func discoveredWorldTarget(world rooms.World, scopedTargetID string) string {
	if scopedTargetID != "" && containsID(world.TargetIDs, scopedTargetID) {
		return scopedTargetID
	}
	for _, targetID := range world.AvailableTargetIDs {
		if targetID == "local" {
			return targetID
		}
	}
	if len(world.AvailableTargetIDs) > 0 {
		return world.AvailableTargetIDs[0]
	}
	for _, targetID := range world.TargetIDs {
		if targetID == "local" {
			return targetID
		}
	}
	if len(world.TargetIDs) > 0 {
		return world.TargetIDs[0]
	}
	return ""
}

func containsID(values []string, expected string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == expected {
			return true
		}
	}
	return false
}

func uniqueIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (s *Service) roomCatalog() (map[string]rooms.Room, error) {
	items, err := s.rooms.List()
	if err != nil {
		return nil, err
	}
	result := make(map[string]rooms.Room, len(items))
	for _, item := range items {
		result[item.ID] = item
	}
	return result, nil
}

func placementRuntimeStatus(placement topology.Placement, target topology.TargetSummary) (string, bool) {
	if placement.State == topology.PlacementConflict {
		return "failed", false
	}
	if target.ID == "" || !target.Online || !target.Configured || !target.InventoryAvailable || target.InventoryStale {
		return "unknown", false
	}
	if placement.Running {
		return "running", true
	}
	return "stopped", true
}

func placementMessage(placement topology.Placement, target topology.TargetSummary) string {
	switch {
	case target.ID == "":
		return "运行目标不存在"
	case !target.Online:
		return "运行目标离线"
	case !target.Configured:
		return "运行目标尚未配置"
	case !target.InventoryAvailable:
		if message := strings.TrimSpace(target.ObservationError); message != "" {
			return message
		}
		return "运行目标清单不可用"
	case target.InventoryStale:
		if message := strings.TrimSpace(target.ObservationError); message != "" {
			return message
		}
		return "运行目标清单已过期"
	case placement.State == topology.PlacementConflict:
		return "世界在多个运行目标上被发现"
	case placement.State == topology.PlacementPlanned:
		return "世界运行位置尚未应用"
	default:
		return ""
	}
}
