package modcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"dont/internal/agents"
	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"
)

type testRoomCatalog struct {
	rooms  []rooms.Room
	worlds map[string][]rooms.World
}

func (c *testRoomCatalog) List() ([]rooms.Room, error) {
	return append([]rooms.Room(nil), c.rooms...), nil
}

func (c *testRoomCatalog) Room(id string) (rooms.Room, error) {
	for _, room := range c.rooms {
		if room.ID == id {
			return room, nil
		}
	}
	return rooms.Room{}, rooms.ErrRoomNotFound
}

func (c *testRoomCatalog) Worlds(roomID string) ([]rooms.World, error) {
	if _, err := c.Room(roomID); err != nil {
		return nil, err
	}
	return append([]rooms.World(nil), c.worlds[roomID]...), nil
}

func (c *testRoomCatalog) World(roomID, worldID string) (rooms.World, error) {
	values, err := c.Worlds(roomID)
	if err != nil {
		return rooms.World{}, err
	}
	for _, world := range values {
		if world.ID == worldID {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

type testPlacementResolver struct {
	mu         sync.Mutex
	executions map[string][]topology.ExecutionPlacement
	calls      map[string]int
}

func (r *testPlacementResolver) ResolveRoomExecutions(_ context.Context, roomID string) ([]topology.ExecutionPlacement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[roomID]++
	values, exists := r.executions[roomID]
	if !exists {
		return nil, rooms.ErrRoomNotFound
	}
	return append([]topology.ExecutionPlacement(nil), values...), nil
}

func (r *testPlacementResolver) count(roomID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[roomID]
}

type testModCatalog struct {
	mu                   sync.Mutex
	dependencyIDs        []string
	configurationContent []byte
	listContents         map[string][]byte
}

func (c *testModCatalog) ResolveDependencies(_ context.Context, modID string, include bool) ([]string, error) {
	if !include || len(c.dependencyIDs) == 0 {
		return []string{modID}, nil
	}
	return append([]string(nil), c.dependencyIDs...), nil
}

func (c *testModCatalog) ConfigurationFromContent(_ context.Context, roomID, worldID, modID string, content []byte) (mods.ModConfiguration, error) {
	snapshot, err := mods.InspectModOverride(content)
	if err != nil {
		return mods.ModConfiguration{}, err
	}
	c.mu.Lock()
	c.configurationContent = append([]byte(nil), content...)
	c.mu.Unlock()
	return mods.ModConfiguration{
		Revision: snapshot.Revision, RoomID: roomID, WorldID: worldID, ModID: modID,
		Fields: []mods.ConfigField{{
			Key: "mode", Label: "模式", Type: "string",
			Options: []mods.ConfigOption{{Value: "easy", Label: "简单"}, {Value: "hard", Label: "困难"}},
		}},
	}, nil
}

func (c *testModCatalog) ListFromOverrides(_ context.Context, _ string, contents map[string][]byte) (mods.ModList, error) {
	c.mu.Lock()
	c.listContents = cloneContents(contents)
	c.mu.Unlock()
	return mods.ModList{Total: len(contents)}, nil
}

func cloneContents(values map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(values))
	for key, value := range values {
		result[key] = append([]byte(nil), value...)
	}
	return result
}

type testModDriver struct {
	mu sync.Mutex

	overrides     map[string][]byte
	overrideCalls []runtimedriver.Target
	overrideHook  func(runtimedriver.Target, string, string, int64) (shared.RuntimeModOverridesChunk, error)

	availableBytes int64
	runtimeVersion string
	inspect        map[string]shared.RuntimeModCacheManifest

	cacheDescriptor runtimedriver.ModUploadDescriptor
	cacheData       []byte
	cacheBegin      int64
	cacheTarget     runtimedriver.Target
	cacheOperation  runtimedriver.Operation
	cacheChunkSizes []int
	cacheCommit     *shared.RuntimeModCacheManifest
	cacheWriteHook  func(runtimedriver.ModUploadDescriptor, int64, []byte) (int64, error)

	planDescriptor runtimedriver.ModUploadDescriptor
	planData       []byte
	planBegin      int64
	planTarget     runtimedriver.Target
	planOperation  runtimedriver.Operation
	planChunkSizes []int
	planWriteHook  func(runtimedriver.ModUploadDescriptor, int64, []byte) (int64, error)

	operations   []string
	runtimeCalls []runtimedriver.Operation
}

func (d *testModDriver) ObserveModTarget(context.Context, runtimedriver.Target) (int64, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.availableBytes, d.runtimeVersion, nil
}

func (d *testModDriver) InspectModCache(_ context.Context, _ runtimedriver.Target, workshopID, treeSHA string) (shared.RuntimeModCacheManifest, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	value, exists := d.inspect[workshopID+"\x00"+treeSHA]
	if !exists {
		return shared.RuntimeModCacheManifest{}, errors.New("cache miss")
	}
	return value, nil
}

func (d *testModDriver) BeginModUpload(_ context.Context, target runtimedriver.Target, operation runtimedriver.Operation, descriptor runtimedriver.ModUploadDescriptor) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cacheBegin < 0 || d.cacheBegin > int64(len(d.cacheData)) {
		return 0, errors.New("invalid cache resume fixture")
	}
	d.cacheDescriptor, d.cacheTarget, d.cacheOperation = descriptor, target, operation
	d.runtimeCalls = append(d.runtimeCalls, operation)
	d.cacheData = d.cacheData[:int(d.cacheBegin)]
	return d.cacheBegin, nil
}

func (d *testModDriver) WriteModUpload(_ context.Context, _ runtimedriver.Target, operation runtimedriver.Operation, descriptor runtimedriver.ModUploadDescriptor, offset int64, data []byte) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtimeCalls = append(d.runtimeCalls, operation)
	if d.cacheWriteHook != nil {
		return d.cacheWriteHook(descriptor, offset, data)
	}
	if offset != int64(len(d.cacheData)) {
		return int64(len(d.cacheData)), errors.New("cache offset mismatch")
	}
	d.cacheData = append(d.cacheData, data...)
	d.cacheChunkSizes = append(d.cacheChunkSizes, len(data))
	return int64(len(d.cacheData)), nil
}

func (d *testModDriver) CommitModUpload(_ context.Context, _ runtimedriver.Target, operation runtimedriver.Operation, descriptor runtimedriver.ModUploadDescriptor) (shared.RuntimeModCacheManifest, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtimeCalls = append(d.runtimeCalls, operation)
	if int64(len(d.cacheData)) != descriptor.Size || testSHA(d.cacheData) != descriptor.SHA256 {
		return shared.RuntimeModCacheManifest{}, errors.New("cache upload integrity mismatch")
	}
	if d.cacheCommit != nil {
		return *d.cacheCommit, nil
	}
	manifest := d.inspect[descriptor.WorkshopID+"\x00manifest"]
	return shared.RuntimeModCacheManifest{
		WorkshopID: descriptor.WorkshopID, TreeSHA256: descriptor.ExpectedTreeSHA256,
		ManifestSHA256: manifest.ManifestSHA256, Size: manifest.Size, FileCount: manifest.FileCount,
	}, nil
}

func (d *testModDriver) BeginModReleasePlan(_ context.Context, target runtimedriver.Target, operation runtimedriver.Operation, descriptor runtimedriver.ModUploadDescriptor) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.planBegin < 0 || d.planBegin > int64(len(d.planData)) {
		return 0, errors.New("invalid plan resume fixture")
	}
	d.planDescriptor, d.planTarget, d.planOperation = descriptor, target, operation
	d.runtimeCalls = append(d.runtimeCalls, operation)
	d.planData = d.planData[:int(d.planBegin)]
	return d.planBegin, nil
}

func (d *testModDriver) WriteModReleasePlan(_ context.Context, _ runtimedriver.Target, operation runtimedriver.Operation, descriptor runtimedriver.ModUploadDescriptor, offset int64, data []byte) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtimeCalls = append(d.runtimeCalls, operation)
	if d.planWriteHook != nil {
		return d.planWriteHook(descriptor, offset, data)
	}
	if offset != int64(len(d.planData)) {
		return int64(len(d.planData)), errors.New("plan offset mismatch")
	}
	d.planData = append(d.planData, data...)
	d.planChunkSizes = append(d.planChunkSizes, len(data))
	return int64(len(d.planData)), nil
}

func (d *testModDriver) CommitModReleasePlan(_ context.Context, _ runtimedriver.Target, operation runtimedriver.Operation, descriptor runtimedriver.ModUploadDescriptor) (shared.RuntimeModReleaseState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtimeCalls = append(d.runtimeCalls, operation)
	if int64(len(d.planData)) != descriptor.Size || testSHA(d.planData) != descriptor.SHA256 {
		return shared.RuntimeModReleaseState{}, errors.New("plan upload integrity mismatch")
	}
	d.operations = append(d.operations, "plan-commit")
	return shared.RuntimeModReleaseState{OperationID: descriptor.OperationID, Phase: "planned"}, nil
}

func (d *testModDriver) PrepareModRelease(_ context.Context, _ runtimedriver.Target, operation runtimedriver.Operation, operationID string) (shared.RuntimeModReleaseState, error) {
	return d.release(operation, operationID, "prepare", "prepared"), nil
}

func (d *testModDriver) PublishModRelease(_ context.Context, _ runtimedriver.Target, operation runtimedriver.Operation, operationID string) (shared.RuntimeModReleaseState, error) {
	return d.release(operation, operationID, "publish", "published"), nil
}

func (d *testModDriver) RollbackModRelease(_ context.Context, _ runtimedriver.Target, operation runtimedriver.Operation, operationID string) (shared.RuntimeModReleaseState, error) {
	return d.release(operation, operationID, "rollback", "rolled_back"), nil
}

func (d *testModDriver) CompleteModRelease(_ context.Context, _ runtimedriver.Target, operation runtimedriver.Operation, operationID string) (shared.RuntimeModReleaseState, error) {
	return d.release(operation, operationID, "complete", "committed"), nil
}

func (d *testModDriver) ModReleaseState(_ context.Context, _ runtimedriver.Target, operationID string) (shared.RuntimeModReleaseState, error) {
	return shared.RuntimeModReleaseState{OperationID: operationID}, nil
}

func (d *testModDriver) ReadModOverrides(_ context.Context, target runtimedriver.Target, roomDirectory, worldDirectory string, offset int64) (shared.RuntimeModOverridesChunk, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.overrideCalls = append(d.overrideCalls, target)
	if d.overrideHook != nil {
		return d.overrideHook(target, roomDirectory, worldDirectory, offset)
	}
	data, exists := d.overrides[roomDirectory+"\x00"+worldDirectory]
	if !exists {
		return shared.RuntimeModOverridesChunk{}, errors.New("remote overrides missing")
	}
	if offset < 0 || offset > int64(len(data)) {
		return shared.RuntimeModOverridesChunk{}, errors.New("invalid read offset")
	}
	end := offset + shared.MaxChunkBytes
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return shared.RuntimeModOverridesChunk{
		RoomDirectory: roomDirectory, WorldDirectory: worldDirectory,
		Offset: offset, NextOffset: end, Size: int64(len(data)), SHA256: testSHA(data),
		Data: append([]byte(nil), data[offset:end]...), Complete: end == int64(len(data)),
	}, nil
}

func (d *testModDriver) release(operation runtimedriver.Operation, operationID, call, phase string) shared.RuntimeModReleaseState {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtimeCalls = append(d.runtimeCalls, operation)
	d.operations = append(d.operations, call)
	return shared.RuntimeModReleaseState{OperationID: operationID, Phase: phase}
}

func testSHA(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

type snapshotCoordinator struct {
	source       *SnapshotSource
	mu           sync.Mutex
	lastWorlds   []modpublication.ManagedWorld
	current      modpublication.Publication
	previewPlan  *modpublication.Plan
	published    []modpublication.PublishRequest
	recoveredIDs []string
}

func (c *snapshotCoordinator) Preview(ctx context.Context, roomID string) (modpublication.Plan, error) {
	if c.previewPlan != nil {
		return *c.previewPlan, nil
	}
	worlds, err := c.source.ManagedWorlds(ctx)
	if err != nil {
		return modpublication.Plan{}, err
	}
	placements, err := c.source.AppliedPlacements(ctx)
	if err != nil {
		return modpublication.Plan{}, err
	}
	c.mu.Lock()
	c.lastWorlds = cloneManagedWorlds(worlds)
	c.mu.Unlock()
	return modpublication.Plan{
		Version: modpublication.PlanVersion, RoomID: roomID, TopologyRevision: placements.TopologyRevision,
		PlanHash: testSHA([]byte(roomID + placements.TopologyRevision)), Ready: true, RestartRequired: true,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (c *snapshotCoordinator) Publish(_ context.Context, request modpublication.PublishRequest) (modpublication.Publication, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.published = append(c.published, request)
	return modpublication.Publication{ID: request.ID, SourceJobID: request.SourceJobID, RoomID: request.Plan.RoomID, Plan: request.Plan, Status: modpublication.StatusSucceeded}, nil
}

func (c *snapshotCoordinator) Get(string) (modpublication.Publication, error) {
	return c.current, nil
}

func (c *snapshotCoordinator) List(string, int, int) ([]modpublication.Publication, int, error) {
	return nil, 0, nil
}

func (c *snapshotCoordinator) Recover(context.Context) ([]modpublication.Publication, error) {
	return nil, nil
}

func (c *snapshotCoordinator) RecoverOne(_ context.Context, id string) (modpublication.Publication, error) {
	c.recoveredIDs = append(c.recoveredIDs, id)
	value := c.current
	value.Status = modpublication.StatusSucceeded
	return value, nil
}

func testExecution(room rooms.Room, world rooms.World, revision, targetID, nodeID, installationID string) topology.ExecutionPlacement {
	target := agents.RuntimeTarget{
		ID: targetID, AgentID: nodeID, Online: true, Configured: true,
		Capabilities: []string{modpublication.RequiredCapability},
		Config:       agents.RuntimeConfig{InstallationID: installationID},
	}
	return topology.ExecutionPlacement{
		Room: room, World: world, Revision: revision, AppliedTargetID: targetID,
		DesiredTargetID: targetID, Target: target,
	}
}

func sortedModStates(t interface{ Fatalf(string, ...interface{}) }, content []byte) []mods.OverrideModState {
	snapshot, err := mods.InspectModOverride(content)
	if err != nil {
		t.Fatalf("inspect mod overrides: %v", err)
	}
	states := append([]mods.OverrideModState(nil), snapshot.Mods...)
	sort.Slice(states, func(i, j int) bool { return states[i].ModID < states[j].ModID })
	return states
}

func decodePlan(data []byte) (shared.RuntimeModPlanInput, error) {
	var value shared.RuntimeModPlanInput
	err := json.Unmarshal(data, &value)
	return value, err
}
