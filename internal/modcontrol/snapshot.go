package modcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"
)

type proposalContextKey struct{}
type snapshotContextKey struct{}

type proposal struct {
	roomID    string
	roomOnly  bool
	action    mods.OverrideAction
	modID     string
	modIDs    []string
	worldIDs  map[string]bool
	enabled   bool
	revision  string
	patch     map[string]json.RawMessage
	overrides map[string][]byte
}

type snapshotState struct {
	mu      sync.Mutex
	loaded  bool
	loading bool
	ready   chan struct{}
	value   runtimeSnapshot
	err     error
}

type runtimeSnapshot struct {
	worlds        []modpublication.ManagedWorld
	placements    modpublication.PlacementSnapshot
	roomRevisions map[string]string
	executions    map[string]topology.ExecutionPlacement
	contents      map[string][]byte
}

type resolvedRoomSnapshot struct {
	room       rooms.Room
	revision   string
	executions []topology.ExecutionPlacement
}

type SnapshotSource struct {
	rooms    RoomCatalog
	topology PlacementResolver
	mods     ModCatalog
	remote   runtimedriver.ModDriver
	saveRoot string
}

func NewSnapshotSource(roomCatalog RoomCatalog, placements PlacementResolver, modCatalog ModCatalog, remote runtimedriver.ModDriver, saveRoot string) (*SnapshotSource, error) {
	if roomCatalog == nil || placements == nil || modCatalog == nil || remote == nil {
		return nil, ErrInvalidRequest
	}
	absolute, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return nil, ErrInvalidRequest
	}
	return &SnapshotSource{rooms: roomCatalog, topology: placements, mods: modCatalog, remote: remote, saveRoot: filepath.Clean(absolute)}, nil
}

func withPublicationSnapshot(ctx context.Context, value proposal) context.Context {
	ctx = context.WithValue(ctx, proposalContextKey{}, value)
	return context.WithValue(ctx, snapshotContextKey{}, &snapshotState{})
}

func (s *SnapshotSource) ManagedWorlds(ctx context.Context) ([]modpublication.ManagedWorld, error) {
	value, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	return cloneManagedWorlds(value.worlds), nil
}

func (s *SnapshotSource) AppliedPlacements(ctx context.Context) (modpublication.PlacementSnapshot, error) {
	value, err := s.load(ctx)
	if err != nil {
		return modpublication.PlacementSnapshot{}, err
	}
	return modpublication.PlacementSnapshot{
		TopologyRevision: value.placements.TopologyRevision,
		Placements:       append([]modpublication.AppliedPlacement(nil), value.placements.Placements...),
	}, nil
}

func (s *SnapshotSource) Contents(ctx context.Context, roomID string) (map[string][]byte, error) {
	value, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte)
	for key, content := range value.contents {
		parts := strings.SplitN(key, "\x00", 2)
		if len(parts) == 2 && parts[0] == roomID {
			result[parts[1]] = append([]byte(nil), content...)
		}
	}
	return result, nil
}

func (s *SnapshotSource) Execution(ctx context.Context, targetID, installationID string) (topology.ExecutionPlacement, bool, error) {
	value, err := s.load(ctx)
	if err != nil {
		return topology.ExecutionPlacement{}, false, err
	}
	execution, ok := value.executions[targetID+"\x00"+installationID]
	return execution, ok, nil
}

func (s *SnapshotSource) RoomTopologyRevision(ctx context.Context, roomID string) (string, bool, error) {
	value, err := s.load(ctx)
	if err != nil {
		return "", false, err
	}
	revision, ok := value.roomRevisions[roomID]
	return revision, ok, nil
}

func (s *SnapshotSource) load(ctx context.Context) (runtimeSnapshot, error) {
	state, _ := ctx.Value(snapshotContextKey{}).(*snapshotState)
	if state == nil {
		return runtimeSnapshot{}, ErrInvalidRequest
	}
	state.mu.Lock()
	if state.loaded {
		value, err := state.value, state.err
		state.mu.Unlock()
		return value, err
	}
	if state.loading {
		ready := state.ready
		state.mu.Unlock()
		select {
		case <-ctx.Done():
			return runtimeSnapshot{}, ctx.Err()
		case <-ready:
			state.mu.Lock()
			value, err := state.value, state.err
			state.mu.Unlock()
			return value, err
		}
	}
	state.loading, state.ready = true, make(chan struct{})
	ready := state.ready
	state.mu.Unlock()
	value, err := s.build(ctx)
	state.mu.Lock()
	state.value, state.err, state.loaded, state.loading = value, err, true, false
	close(ready)
	state.mu.Unlock()
	return value, err
}

func (s *SnapshotSource) build(ctx context.Context) (runtimeSnapshot, error) {
	request, _ := ctx.Value(proposalContextKey{}).(proposal)
	roomValues, err := s.rooms.List()
	if err != nil {
		return runtimeSnapshot{}, err
	}
	result := runtimeSnapshot{
		roomRevisions: make(map[string]string), executions: make(map[string]topology.ExecutionPlacement),
		contents: make(map[string][]byte),
	}
	resolvedRooms := make([]resolvedRoomSnapshot, 0, len(roomValues))
	for _, room := range roomValues {
		if !room.Managed || request.roomOnly && room.ID != request.roomID {
			continue
		}
		executions, err := s.topology.ResolveRoomExecutions(ctx, room.ID)
		if err != nil {
			return runtimeSnapshot{}, err
		}
		roomRevision := ""
		worldsSeen := make(map[string]bool, len(executions))
		for _, execution := range executions {
			if err := ctx.Err(); err != nil {
				return runtimeSnapshot{}, err
			}
			if execution.Room.ID != room.ID || execution.World.RoomID != room.ID || worldsSeen[execution.World.ID] || strings.TrimSpace(execution.Revision) == "" {
				return runtimeSnapshot{}, ErrTopologyChanged
			}
			worldsSeen[execution.World.ID] = true
			if roomRevision == "" {
				roomRevision = execution.Revision
			} else if roomRevision != execution.Revision {
				return runtimeSnapshot{}, ErrTopologyChanged
			}
		}
		if roomRevision != "" {
			result.roomRevisions[room.ID] = roomRevision
		}
		resolvedRooms = append(resolvedRooms, resolvedRoomSnapshot{room: room, revision: roomRevision, executions: executions})
	}

	filterInstallations := !request.roomOnly && request.roomID != ""
	requestedInstallations := make(map[string]bool)
	if filterInstallations {
		for _, resolved := range resolvedRooms {
			if resolved.room.ID != request.roomID {
				continue
			}
			for _, execution := range resolved.executions {
				placement := publicationPlacement(execution, resolved.room.ID, execution.World.ID)
				requestedInstallations[placement.TargetID+"\x00"+placement.InstallationID] = true
			}
		}
	}

	revisions := make([]string, 0, len(resolvedRooms))
	selectedWorlds := make(map[string]bool)
	requestedMods := make(map[string]bool)
	for _, resolved := range resolvedRooms {
		roomIncluded := false
		for _, execution := range resolved.executions {
			if err := ctx.Err(); err != nil {
				return runtimeSnapshot{}, err
			}
			world := execution.World
			room := resolved.room
			placement := publicationPlacement(execution, room.ID, world.ID)
			installationKey := placement.TargetID + "\x00" + placement.InstallationID
			if filterInstallations && !requestedInstallations[installationKey] {
				continue
			}
			roomIncluded = true
			content, err := s.readOverrides(ctx, execution, placement)
			if err != nil {
				return runtimeSnapshot{}, fmt.Errorf("read room %s world %s modoverrides.lua: %w", room.ID, world.ID, err)
			}
			if next, ok := request.overrides[room.ID+"\x00"+world.ID]; ok {
				content = append([]byte(nil), next...)
			} else if room.ID == request.roomID && request.worldIDs[world.ID] {
				selectedWorlds[world.ID] = true
				content, err = s.applyProposal(ctx, request, world.ID, content)
				if err != nil {
					return runtimeSnapshot{}, err
				}
			}
			snapshot, err := mods.InspectModOverride(content)
			if err != nil {
				return runtimeSnapshot{}, err
			}
			managed := modpublication.ManagedWorld{
				RoomID: room.ID, RoomDirectory: room.DirectoryName, WorldID: world.ID,
				WorldDirectory: world.DirectoryName, ModOverrides: append([]byte(nil), content...),
			}
			for _, item := range snapshot.Mods {
				managed.Mods = append(managed.Mods, modpublication.ModRequirement{WorkshopID: item.ModID})
				if room.ID == request.roomID {
					requestedMods[item.ModID] = true
				}
			}
			result.worlds = append(result.worlds, managed)
			result.placements.Placements = append(result.placements.Placements, placement)
			if prior, exists := result.executions[installationKey]; exists && !sameRuntimeInstallation(prior, execution) {
				return runtimeSnapshot{}, ErrTopologyChanged
			}
			result.executions[installationKey] = execution
			result.contents[room.ID+"\x00"+world.ID] = append([]byte(nil), content...)
		}
		if roomIncluded && resolved.revision != "" {
			revisions = append(revisions, resolved.room.ID+"\x00"+resolved.revision)
		}
	}
	if request.roomID != "" && len(selectedWorlds) != len(request.worldIDs) {
		return runtimeSnapshot{}, rooms.ErrWorldNotFound
	}
	if request.action == "reconcile" && len(request.modIDs) > 0 && !sameStringSet(request.modIDs, requestedMods) {
		return runtimeSnapshot{}, modpublication.ErrPlanChanged
	}
	sort.Strings(revisions)
	digest := sha256.Sum256([]byte(strings.Join(revisions, "\x00")))
	result.placements.TopologyRevision = hex.EncodeToString(digest[:])
	return result, nil
}

func sameStringSet(expected []string, actual map[string]bool) bool {
	if len(expected) != len(actual) {
		return false
	}
	for _, value := range expected {
		if !actual[value] {
			return false
		}
	}
	return true
}

func sameRuntimeInstallation(left, right topology.ExecutionPlacement) bool {
	return strings.TrimSpace(left.AppliedTargetID) == strings.TrimSpace(right.AppliedTargetID) &&
		strings.TrimSpace(left.Target.ID) == strings.TrimSpace(right.Target.ID) &&
		strings.TrimSpace(left.Target.AgentID) == strings.TrimSpace(right.Target.AgentID) &&
		strings.TrimSpace(left.Target.Config.InstallationID) == strings.TrimSpace(right.Target.Config.InstallationID)
}

func publicationPlacement(execution topology.ExecutionPlacement, roomID, worldID string) modpublication.AppliedPlacement {
	targetID := strings.TrimSpace(execution.AppliedTargetID)
	installationID := strings.TrimSpace(execution.Target.Config.InstallationID)
	nodeID := strings.TrimSpace(execution.Target.AgentID)
	if targetID == "local" {
		nodeID = "local"
		if installationID == "" {
			installationID = "default"
		}
	}
	if nodeID == "" {
		nodeID = targetID
	}
	return modpublication.AppliedPlacement{RoomID: roomID, WorldID: worldID, TargetID: targetID, NodeID: nodeID, InstallationID: installationID}
}

func (s *SnapshotSource) applyProposal(ctx context.Context, request proposal, worldID string, content []byte) ([]byte, error) {
	if request.action == "" {
		return content, nil
	}
	if request.action == "reconcile" {
		return content, nil
	}
	current, err := mods.InspectModOverride(content)
	if err != nil {
		return nil, err
	}
	mutation := mods.OverrideMutation{
		Action: request.action, ModID: request.modID, ModIDs: append([]string(nil), request.modIDs...),
		Enabled: request.enabled, ExpectedRevision: current.Revision,
	}
	if request.action == mods.OverrideActionConfigure {
		mutation.ExpectedRevision = request.revision
		mutation.Patch = make(map[string]json.RawMessage, len(request.patch))
		for key, value := range request.patch {
			mutation.Patch[key] = append(json.RawMessage(nil), value...)
		}
		configuration, err := s.mods.ConfigurationFromContent(ctx, request.roomID, worldID, request.modID, content)
		if err != nil {
			return nil, err
		}
		mutation.Fields = configuration.Fields
	}
	result, err := mods.MutateModOverride(content, mutation)
	if errors.Is(err, mods.ErrNoChanges) {
		return content, nil
	}
	if err != nil {
		return nil, err
	}
	return result.Content, nil
}

func (s *SnapshotSource) readOverrides(ctx context.Context, execution topology.ExecutionPlacement, placement modpublication.AppliedPlacement) ([]byte, error) {
	if placement.TargetID == "local" {
		path := filepath.Join(s.saveRoot, execution.Room.DirectoryName, execution.World.DirectoryName, "modoverrides.lua")
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(err, errors.New("modoverrides.lua is not a trusted regular file"))
		}
		if info.Size() < 0 || info.Size() > 8<<20 {
			return nil, errors.New("modoverrides.lua exceeded its safe read limit")
		}
		return os.ReadFile(path)
	}
	target := runtimedriver.Target{
		TargetID: placement.TargetID, InstallationID: placement.InstallationID,
		RoomID: placement.RoomID, WorldID: placement.WorldID, Cluster: execution.Room.DirectoryName,
		Shard: execution.World.DirectoryName, TopologyRevision: execution.Revision,
	}
	var data []byte
	var size int64 = -1
	var digest string
	for offset := int64(0); ; {
		chunk, err := s.remote.ReadModOverrides(ctx, target, execution.Room.DirectoryName, execution.World.DirectoryName, offset)
		if err != nil {
			return nil, err
		}
		if chunk.Size < 0 || chunk.Size > 8<<20 || len(chunk.Data) > shared.MaxChunkBytes ||
			chunk.Offset != offset || chunk.NextOffset != offset+int64(len(chunk.Data)) || chunk.NextOffset > chunk.Size ||
			chunk.RoomDirectory != execution.Room.DirectoryName || chunk.WorldDirectory != execution.World.DirectoryName {
			return nil, errors.New("remote modoverrides.lua chunk is inconsistent")
		}
		if size < 0 {
			size, digest = chunk.Size, strings.ToLower(chunk.SHA256)
		} else if size != chunk.Size || digest != strings.ToLower(chunk.SHA256) {
			return nil, errors.New("remote modoverrides.lua changed while reading")
		}
		if int64(len(data)) > chunk.Size-int64(len(chunk.Data)) {
			return nil, errors.New("remote modoverrides.lua chunk is inconsistent")
		}
		data = append(data, chunk.Data...)
		offset = chunk.NextOffset
		if chunk.Complete {
			if offset != size || shaHex(data) != digest {
				return nil, errors.New("remote modoverrides.lua integrity check failed")
			}
			return data, nil
		}
		if len(chunk.Data) == 0 {
			return nil, errors.New("remote modoverrides.lua exceeded its safe read limit")
		}
	}
}

func shaHex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func cloneManagedWorlds(values []modpublication.ManagedWorld) []modpublication.ManagedWorld {
	result := make([]modpublication.ManagedWorld, len(values))
	for index, value := range values {
		result[index] = value
		result[index].Mods = append([]modpublication.ModRequirement(nil), value.Mods...)
		result[index].ModOverrides = append([]byte(nil), value.ModOverrides...)
	}
	return result
}
