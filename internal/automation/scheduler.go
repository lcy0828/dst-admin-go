package automation

import (
	"context"
	"fmt"
	"sync"

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
	s.mu.Lock()
	engine := s.engine
	s.engine, s.started = nil, false
	s.mu.Unlock()
	if engine != nil {
		engine.Stop()
	}
}

func (s *Scheduler) build() (*cron.Cron, error) {
	engine := cron.New()
	tasks, err := s.store.ScheduledTasks()
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		taskID, roomID := task.ID, task.RoomID
		spec := "CRON_TZ=" + task.Timezone + " " + task.Schedule
		if _, err := engine.AddFunc(spec, func() { _, _ = s.service.RunTask(roomID, taskID, TriggerSchedule) }); err != nil {
			return nil, fmt.Errorf("schedule automation task %s: %w", task.ID, err)
		}
	}
	return engine, nil
}
