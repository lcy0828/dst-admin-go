package automation

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"dont/internal/rooms"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	store   *Store
	service *Service
	mu      sync.Mutex
	engine  *cron.Cron
	started bool
}

func NewScheduler(store *Store, service *Service) *Scheduler {
	return &Scheduler{store: store, service: service}
}

func (s *Scheduler) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	if err := s.store.RecoverRuns(); err != nil {
		return err
	}
	engine, err := s.build()
	if err != nil {
		return err
	}
	engine.Start()
	s.engine, s.started = engine, true
	return nil
}

func (s *Scheduler) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	engine, err := s.build()
	if err != nil {
		return err
	}
	engine.Start()
	old := s.engine
	s.engine, s.started = engine, true
	if old != nil {
		old.Stop()
	}
	return nil
}

func (s *Scheduler) Stop() {
	_ = s.Close(context.Background())
}

func (s *Scheduler) Close(ctx context.Context) error {
	s.mu.Lock()
	engine := s.engine
	s.engine, s.started = nil, false
	s.mu.Unlock()
	if engine == nil {
		return nil
	}
	stopped := engine.Stop()
	select {
	case <-stopped.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) build() (*cron.Cron, error) {
	engine := cron.New(cron.WithParser(scheduleParser))
	tasks, err := s.store.ScheduledTasks()
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		room, roomErr := s.service.rooms.Room(task.RoomID)
		if errors.Is(roomErr, rooms.ErrRoomNotFound) || errors.Is(roomErr, rooms.ErrInvalidRoom) {
			continue
		}
		if roomErr != nil {
			return nil, fmt.Errorf("inspect automation room %s: %w", task.RoomID, roomErr)
		}
		if !room.Managed {
			continue
		}
		taskID, roomID := task.ID, task.RoomID
		spec := "CRON_TZ=" + task.Timezone + " " + task.Schedule
		if _, err := engine.AddFunc(spec, func() { _, _ = s.service.RunTask(roomID, taskID, TriggerSchedule) }); err != nil {
			return nil, fmt.Errorf("schedule automation task %s: %w", task.ID, err)
		}
	}
	return engine, nil
}
