package runtimeaudit

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"dont/internal/rooms"
	"dont/internal/shards"
)

var ErrInvalidFilter = errors.New("runtime audit filter is invalid")

type RoomCatalog interface {
	List() ([]rooms.Room, error)
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
	World(string, string) (rooms.World, error)
}

type Runtime interface {
	Status(context.Context, string, string) (shards.RuntimeStatus, error)
}

type IdentifiedRuntime interface {
	StatusFor(context.Context, string, string) (shards.RuntimeStatus, error)
}

type observedRuntime struct {
	state         shards.RuntimeState
	sessionExists bool
}

type Service struct {
	rooms     RoomCatalog
	runtime   Runtime
	store     *Store
	now       func() time.Time
	wake      chan struct{}
	watchMu   sync.Mutex
	fastUntil time.Time
}

const fastPollingWindow = 2 * time.Minute

func NewService(roomCatalog RoomCatalog, runtime Runtime, store *Store) (*Service, error) {
	if roomCatalog == nil || runtime == nil || store == nil {
		return nil, errors.New("runtime audit dependencies are required")
	}
	return &Service{rooms: roomCatalog, runtime: runtime, store: store, now: time.Now, wake: make(chan struct{}, 1)}, nil
}

func (s *Service) RecordAction(request ActionRequest) error {
	room, err := s.rooms.Room(request.RoomID)
	if err != nil {
		return err
	}
	worlds, err := s.rooms.Worlds(room.ID)
	if err != nil {
		return err
	}
	selected := make(map[string]bool, len(request.WorldIDs))
	for _, worldID := range request.WorldIDs {
		selected[worldID] = true
	}
	eventType, expectedExit := actionEvent(request.Action)
	for _, world := range worlds {
		if len(selected) > 0 && !selected[world.ID] {
			continue
		}
		status, statusErr := s.status(context.Background(), room, world)
		if statusErr != nil {
			status = shards.RuntimeStatus{State: shards.RuntimeUnknown}
		}
		_, err = s.store.Append(Event{
			RoomID: room.ID, WorldID: world.ID, RoomDirectory: room.DirectoryName, WorldDirectory: world.DirectoryName,
			Type: eventType, Action: request.Action, Source: request.Source, RuntimeState: string(status.State),
			JobID: request.JobID, RequestID: request.RequestID, ExpectedExit: expectedExit && status.SessionExists, OccurredAt: s.now().UTC(),
		})
		if err != nil {
			return err
		}
	}
	s.requestFastPolling()
	return nil
}

func (s *Service) List(roomID string, filter ListFilter) (List, error) {
	if filter.Limit < 1 || filter.Limit > 200 {
		return List{}, ErrInvalidFilter
	}
	if _, err := s.rooms.Room(roomID); err != nil {
		return List{}, err
	}
	if filter.WorldID != "" {
		if _, err := s.rooms.World(roomID, filter.WorldID); err != nil {
			return List{}, err
		}
	}
	return s.store.List(roomID, filter)
}

func (s *Service) LatestExit(roomID, worldID string) (*Event, error) {
	return s.store.LatestExit(roomID, worldID)
}

func (s *Service) ObserveOperation(_ context.Context, audit shards.OperationAudit) error {
	err := s.store.AnnotateAction(Event{
		RoomID: audit.RoomID, WorldID: audit.WorldID, Action: string(audit.Action), Source: Source(audit.Source),
		JobID: audit.JobID, RequestID: audit.RequestID, TargetID: audit.TargetID, AgentID: audit.AgentID,
		OperationID: audit.OperationID, OperationKey: audit.OperationKey, LeaseID: audit.LeaseID,
		FencingToken: audit.FencingToken, TopologyRevision: audit.TopologyRevision,
	})
	if err == nil {
		s.requestFastPolling()
	}
	return err
}

func (s *Service) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	previous := s.observe(ctx, nil, false)
	timer := time.NewTimer(s.pollDelay(previous, interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			previous = s.observe(ctx, previous, true)
		case <-timer.C:
			previous = s.observe(ctx, previous, true)
		}
		resetTimer(timer, s.pollDelay(previous, interval))
	}
}

func (s *Service) requestFastPolling() {
	now := s.now().UTC()
	s.watchMu.Lock()
	until := now.Add(fastPollingWindow)
	if until.After(s.fastUntil) {
		s.fastUntil = until
	}
	s.watchMu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) pollDelay(states map[string]observedRuntime, fast time.Duration) time.Duration {
	if fast <= 0 {
		fast = 5 * time.Second
	}
	s.watchMu.Lock()
	fastUntil := s.fastUntil
	s.watchMu.Unlock()
	if s.now().UTC().Before(fastUntil) || len(states) == 0 {
		return fast
	}
	allStopped := true
	for _, state := range states {
		switch state.state {
		case shards.RuntimeRunning:
			allStopped = false
		case shards.RuntimeStopped:
		default:
			return fast
		}
	}
	if allStopped {
		return 12 * fast
	}
	return 6 * fast
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func (s *Service) observe(ctx context.Context, previous map[string]observedRuntime, emit bool) map[string]observedRuntime {
	current := make(map[string]observedRuntime)
	roomItems, err := s.rooms.List()
	if err != nil {
		log.Printf("[RuntimeAudit] list rooms: %v", err)
		return previous
	}
	for _, room := range roomItems {
		if !room.Managed {
			continue
		}
		worlds, worldsErr := s.rooms.Worlds(room.ID)
		if worldsErr != nil {
			continue
		}
		for _, world := range worlds {
			if ctx.Err() != nil {
				return current
			}
			status, statusErr := s.status(ctx, room, world)
			if statusErr != nil {
				continue
			}
			key := room.ID + "\x00" + world.ID
			next := observedRuntime{state: status.State, sessionExists: status.SessionExists}
			current[key] = next
			before, existed := previous[key]
			if !emit || !existed {
				continue
			}
			s.recordTransition(room, world, before, next, status)
		}
	}
	return current
}

func (s *Service) status(ctx context.Context, room rooms.Room, world rooms.World) (shards.RuntimeStatus, error) {
	if runtime, ok := s.runtime.(IdentifiedRuntime); ok {
		return runtime.StatusFor(ctx, room.ID, world.ID)
	}
	return s.runtime.Status(ctx, room.DirectoryName, world.DirectoryName)
}

func (s *Service) recordTransition(room rooms.Room, world rooms.World, before, current observedRuntime, status shards.RuntimeStatus) {
	now := s.now().UTC()
	base := Event{
		RoomID: room.ID, WorldID: world.ID, RoomDirectory: room.DirectoryName, WorldDirectory: world.DirectoryName,
		PreviousState: string(before.state), RuntimeState: string(current.state), Source: SourceSystemMonitor, OccurredAt: now,
	}
	if before.sessionExists && !current.sessionExists {
		expected, err := s.store.ConsumeExpectedExit(room.ID, world.ID, now.Add(-10*time.Minute))
		if err != nil {
			log.Printf("[RuntimeAudit] consume expected exit room=%s world=%s: %v", room.ID, world.ID, err)
			return
		}
		if expected != nil {
			base.Type, base.Action, base.Source = EventStopped, expected.Action, expected.Source
			base.JobID, base.RequestID = expected.JobID, expected.RequestID
			base.TargetID, base.AgentID = expected.TargetID, expected.AgentID
			base.OperationID, base.OperationKey = expected.OperationID, expected.OperationKey
			base.LeaseID, base.FencingToken, base.TopologyRevision = expected.LeaseID, expected.FencingToken, expected.TopologyRevision
			base.ReasonCode, base.Message = "EXPECTED_EXIT", "分片在停止、重启或清理请求后退出"
		} else if status.Code == "CONTAINER_EXIT_CLEAN" {
			base.Type, base.Action, base.Source = EventStopped, "external_shutdown", SourceExternal
			base.ReasonCode, base.Message = status.Code, status.Message
			base.ExpectedExit, base.ExpectedObserved = true, true
		} else if status.Code == "CONTAINER_CREATED" {
			base.Type, base.Action, base.Source = EventStopped, "external_recreate", SourceExternal
			base.ReasonCode = "CONTAINER_REPLACED"
			base.Message = "检测到分片容器已重新创建；旧实例的退出状态未被观测，不能判定为异常退出"
		} else {
			base.Type, base.Source = EventUnexpectedExit, SourceExternal
			base.ReasonCode, base.Message = "SESSION_DISAPPEARED", "未发现对应的停止、重启或清理请求，tmux 会话已消失"
		}
		if _, err := s.store.Append(base); err != nil {
			log.Printf("[RuntimeAudit] append exit room=%s world=%s: %v", room.ID, world.ID, err)
		}
		return
	}
	if !before.sessionExists && current.sessionExists {
		base.Type, base.ReasonCode, base.Message = EventSessionStarted, "SESSION_CREATED", "检测到 tmux 会话已创建"
		_, _ = s.store.Append(base)
	}
	if before.state == current.state {
		return
	}
	switch current.state {
	case shards.RuntimeRunning:
		base.Type, base.ReasonCode, base.Message = EventRunning, "DST_READY", "DST 已完成世界加载和服务注册"
	case shards.RuntimeFailed:
		base.Type, base.ReasonCode, base.Message = EventFailed, status.Code, status.Message
	default:
		return
	}
	if _, err := s.store.Append(base); err != nil {
		log.Printf("[RuntimeAudit] append transition room=%s world=%s: %v", room.ID, world.ID, err)
	}
}

func actionEvent(action string) (EventType, bool) {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "stop":
		return EventStopRequested, true
	case "restart":
		return EventRestartRequested, true
	case "save":
		return EventSaveRequested, false
	case "cleanup":
		return EventCleanupRequested, true
	default:
		return EventStartRequested, false
	}
}
