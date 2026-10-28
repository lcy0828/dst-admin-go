package dstruntime

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"dont/internal/jobs"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/shared"
)

type MaintenanceCatalog interface {
	Catalog
	List() ([]rooms.Room, error)
}
type MaintenanceRuntime interface {
	EnsureLuaRuntime(context.Context, string, string, shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error)
}

type maintenanceScopeResolver interface {
	// LuaRuntimeScope identifies the applied placement and node installation.
	// It reads control-plane metadata only, without collecting runtime inventory.
	LuaRuntimeScope(string, string) (string, error)
}

type UpdateReport struct {
	State             string    `json:"state"`
	Message           string    `json:"message"`
	Version           string    `json:"version,omitempty"`
	InstallationState string    `json:"installationState,omitempty"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// AutoUpdater has no polling loop. Startup, node generation/capability changes,
// and explicit room adoption wake a single bounded maintenance worker.
type AutoUpdater struct {
	catalog      MaintenanceCatalog
	runtime      MaintenanceRuntime
	wake         chan struct{}
	mu           sync.RWMutex
	reports      map[string]*maintenanceUpdate
	requested    map[string]maintenanceTarget
	allRequested bool
}

type maintenanceTarget struct {
	roomID, worldID string
	attempts        int
	next            time.Time
}

type maintenanceUpdate struct {
	scope  string
	active bool
	report UpdateReport
}

func NewAutoUpdater(catalog MaintenanceCatalog, runtime MaintenanceRuntime) *AutoUpdater {
	return &AutoUpdater{catalog: catalog, runtime: runtime, wake: make(chan struct{}, 1), reports: make(map[string]*maintenanceUpdate)}
}

func (u *AutoUpdater) Wake() {
	u.mu.Lock()
	u.allRequested = true
	u.mu.Unlock()
	u.signal()
}

func (u *AutoUpdater) signal() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

// WakeWorld queues only the affected world, without touching the fleet catalog
// or doing work inside a lifecycle completion callback.
func (u *AutoUpdater) WakeWorld(roomID, worldID string) {
	if roomID == "" || worldID == "" {
		return
	}
	u.mu.Lock()
	if u.requested == nil {
		u.requested = make(map[string]maintenanceTarget)
	}
	key := roomID + "\x00" + worldID
	if len(u.requested) < 1024 {
		u.requested[key] = maintenanceTarget{roomID: roomID, worldID: worldID, next: time.Now()}
	}
	u.mu.Unlock()
	u.signal()
}

// A previous startup failure can prevent maintenance. Once a lifecycle job
// finishes, retry only the worlds that were actually started successfully.
func (u *AutoUpdater) OnJob(job jobs.Job) {
	switch job.Kind {
	case "room.start", "room.restart", "rooms.start", "rooms.restart":
	default:
		return
	}
	if job.Status != jobs.StatusSucceeded && job.Status != jobs.StatusFailed && job.Status != jobs.StatusCanceled {
		return
	}
	if job.RoomID != "" {
		for _, target := range job.Targets {
			if target.Status == jobs.StatusSucceeded {
				u.WakeWorld(job.RoomID, target.TargetID)
			}
		}
		return
	}
	for _, target := range job.Targets {
		if target.Status != jobs.StatusSucceeded {
			continue
		}
		// PlanBatch persists one target per world as roomID:worldID. Decode
		// that identity even when no optional runtime mode plan was recorded.
		if roomID, worldID, ok := strings.Cut(target.TargetID, ":"); ok {
			u.WakeWorld(roomID, worldID)
		}
	}
}

func (u *AutoUpdater) takeRequests() (bool, map[string]maintenanceTarget) {
	u.mu.Lock()
	defer u.mu.Unlock()
	all, requested := u.allRequested, u.requested
	u.allRequested, u.requested = false, nil
	return all, requested
}
func (u *AutoUpdater) Report(roomID, worldID string) (UpdateReport, bool) {
	u.mu.RLock()
	entry := u.reports[roomID+"\x00"+worldID]
	if entry == nil {
		u.mu.RUnlock()
		return UpdateReport{}, false
	}
	value, observedScope := entry.report, entry.scope
	u.mu.RUnlock()
	scope, err := u.runtimeScope(roomID, worldID)
	return value, err == nil && observedScope == scope
}

func (u *AutoUpdater) runtimeScope(roomID, worldID string) (string, error) {
	if resolver, ok := u.runtime.(maintenanceScopeResolver); ok {
		return resolver.LuaRuntimeScope(roomID, worldID)
	}
	return "", nil
}

func (u *AutoUpdater) begin(roomID, worldID, scope string) (*maintenanceUpdate, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	key := roomID + "\x00" + worldID
	previous := u.reports[key]
	if previous != nil && previous.scope == scope && previous.active {
		// Keep one observation in flight per location. A duplicate busy result
		// must not replace the operation whose result is still on its way back.
		return nil, ErrRuntimeRefreshDeferred
	}
	value := UpdateReport{State: "updating", Message: "正在检查并更新 Runtime", UpdatedAt: time.Now().UTC()}
	if previous != nil && previous.scope == scope {
		value.InstallationState, value.Version = previous.report.InstallationState, previous.report.Version
	}
	entry := &maintenanceUpdate{scope: scope, active: true, report: value}
	if len(u.reports) >= 1024 { // Reporting is bounded; maintenance still covers every world.
		if previous == nil {
			for old, cached := range u.reports {
				if !cached.active {
					delete(u.reports, old)
					break
				}
			}
			if len(u.reports) >= 1024 {
				return nil, ErrRuntimeRefreshDeferred
			}
		}
	}
	u.reports[key] = entry
	return entry, nil
}

func (u *AutoUpdater) complete(roomID, worldID string, attempt *maintenanceUpdate, value UpdateReport) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reports[roomID+"\x00"+worldID] != attempt {
		return // A different placement or node generation superseded this attempt.
	}
	if value.InstallationState == "" {
		// Progress and transport/busy errors are not installation observations.
		value.InstallationState, value.Version = attempt.report.InstallationState, attempt.report.Version
	}
	attempt.active, attempt.report = false, value
}

func (u *AutoUpdater) recordRetry(roomID, worldID string, attempt *maintenanceUpdate, state, message string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if attempt == nil || u.reports[roomID+"\x00"+worldID] != attempt || attempt.active {
		return false
	}
	attempt.report.State, attempt.report.Message = state, message
	attempt.report.UpdatedAt = time.Now().UTC()
	return true
}

func (u *AutoUpdater) EnsureWorld(ctx context.Context, roomID, worldID string, install bool) (shared.RuntimeLuaResult, error) {
	value, _, err := u.ensureWorld(ctx, roomID, worldID, install)
	return value, err
}

func (u *AutoUpdater) ensureWorld(ctx context.Context, roomID, worldID string, install bool) (shared.RuntimeLuaResult, *maintenanceUpdate, error) {
	room, err := u.catalog.Room(roomID)
	if err != nil {
		return shared.RuntimeLuaResult{}, nil, err
	}
	if !room.Managed {
		return shared.RuntimeLuaResult{}, nil, rooms.ErrRoomNotManaged
	}
	if _, err := u.catalog.World(room.ID, worldID); err != nil {
		return shared.RuntimeLuaResult{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return shared.RuntimeLuaResult{}, nil, err
	}
	scope, err := u.runtimeScope(roomID, worldID)
	if err != nil {
		return shared.RuntimeLuaResult{}, nil, err
	}
	attempt, err := u.begin(roomID, worldID, scope)
	if err != nil {
		return shared.RuntimeLuaResult{Deferred: true, Message: "Runtime 检查正在进行，请稍后重试"}, nil, err
	}
	value, err := u.runtime.EnsureLuaRuntime(ctx, roomID, worldID, shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: install})
	report := UpdateReport{State: "current", Message: value.Message, Version: value.Version, InstallationState: value.State, UpdatedAt: time.Now().UTC()}
	if value.Changed {
		report.State = "updated"
	}
	if value.State == string(InstallStateMissing) {
		report.State = "disabled"
	}
	if err != nil {
		report.State, report.Message = "failed", err.Error()
		if errors.Is(err, operationlease.ErrBusy) || errors.Is(err, ErrRuntimeRefreshDeferred) {
			report.State, report.Message = "updating", "当前房间或 Runtime 正忙，更新已暂缓，请稍后重试"
		}
	}
	u.complete(roomID, worldID, attempt, report)
	return value, attempt, err
}

func (u *AutoUpdater) Run(ctx context.Context) {
	pending := make(map[string]maintenanceTarget)
	u.Wake()
	for {
		var selected maintenanceTarget
		var selectedKey string
		for key, value := range pending {
			if selectedKey == "" || value.next.Before(selected.next) {
				selectedKey, selected = key, value
			}
		}
		var timer *time.Timer
		var ready <-chan time.Time
		if selectedKey != "" {
			timer = time.NewTimer(max(time.Until(selected.next), 0))
			ready = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-u.wake:
			if timer != nil {
				timer.Stop()
			}
			all, requested := u.takeRequests()
			for key, value := range requested {
				if _, exists := pending[key]; !exists {
					pending[key] = value
				}
			}
			if !all {
				continue
			}
			items, err := u.catalog.List()
			if err != nil {
				log.Printf("[RuntimeUpdate] list rooms: %v", err)
				continue
			}
			for _, room := range items {
				if !room.Managed {
					continue
				}
				worlds, err := u.catalog.Worlds(room.ID)
				if err != nil {
					log.Printf("[RuntimeUpdate] list worlds: %v", err)
					continue
				}
				for _, world := range worlds {
					key := room.ID + "\x00" + world.ID
					if _, exists := pending[key]; !exists {
						pending[key] = maintenanceTarget{roomID: room.ID, worldID: world.ID, next: time.Now()}
					}
				}
			}
		case <-ready:
			delete(pending, selectedKey)
			attemptCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			_, attempt, err := u.ensureWorld(attemptCtx, selected.roomID, selected.worldID, false)
			cancel()
			if err == nil {
				continue
			}
			if selected.attempts < 8 && (errors.Is(err, operationlease.ErrBusy) || errors.Is(err, ErrRuntimeRefreshDeferred)) {
				// A superseded attempt owns neither the newer report nor its
				// retry schedule. Duplicate callers can still retry independently.
				if attempt != nil && !u.recordRetry(selected.roomID, selected.worldID, attempt, "updating", "等待当前操作结束后自动继续更新 Runtime") {
					continue
				}
				delay := min(time.Second*time.Duration(1<<selected.attempts), 30*time.Second)
				selected.attempts++
				selected.next = time.Now().Add(delay)
				pending[selectedKey] = selected
				continue
			}
			if errors.Is(err, operationlease.ErrBusy) || errors.Is(err, ErrRuntimeRefreshDeferred) {
				u.recordRetry(selected.roomID, selected.worldID, attempt, "failed", "本次自动维护重试已结束，请在当前操作完成后手动重试："+err.Error())
			}
			log.Printf("[RuntimeUpdate] room=%s world=%s: %v", selected.roomID, selected.worldID, err)
		}
	}
}
