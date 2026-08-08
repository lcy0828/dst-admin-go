package containers

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"dont/internal/jobs"
)

var containerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

type Service struct {
	transport Transport
	jobs      *jobs.Service
	now       func() time.Time
	mu        sync.Mutex
	active    map[string]bool
}

func NewService(transport Transport, jobService *jobs.Service) (*Service, error) {
	if transport == nil || jobService == nil {
		return nil, fmt.Errorf("container dependencies are required")
	}
	return &Service{transport: transport, jobs: jobService, now: time.Now, active: make(map[string]bool)}, nil
}

func (s *Service) List(ctx context.Context) List {
	result := List{Available: s.transport.Available(), Items: []Container{}, ObservedAt: s.now().UTC()}
	if !result.Available {
		result.Error = "Docker CLI 不可用"
		return result
	}
	items, err := s.transport.List(ctx)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Items, result.Total = items, len(items)
	return result
}

func (s *Service) Run(containerID string, action Action, input ActionInput) (jobs.Job, error) {
	if !containerIDPattern.MatchString(containerID) || !validAction(action) {
		return jobs.Job{}, ErrInvalidInput
	}
	if !s.transport.Available() {
		return jobs.Job{}, ErrUnavailable
	}
	items, err := s.transport.List(context.Background())
	if err != nil {
		return jobs.Job{}, err
	}
	var target *Container
	for index := range items {
		if items[index].ID == containerID {
			target = &items[index]
			break
		}
	}
	if target == nil {
		return jobs.Job{}, ErrNotFound
	}
	if !target.Managed {
		return jobs.Job{}, ErrInvalidInput
	}
	if action == ActionStart && target.Running || action == ActionStop && !target.Running || action == ActionRemove && target.Running {
		return jobs.Job{}, ErrConflict
	}
	if action == ActionRemove && strings.TrimSpace(input.Confirmation) != target.Name {
		return jobs.Job{}, ErrConfirmationRequired
	}
	s.mu.Lock()
	if s.active[target.ID] {
		s.mu.Unlock()
		return jobs.Job{}, ErrConflict
	}
	s.active[target.ID] = true
	s.mu.Unlock()
	job, err := s.jobs.Submit("container."+string(action), "", "", []jobs.TargetSpec{{ID: target.ID, Name: target.Name}}, func(ctx context.Context, report func(jobs.TargetResult)) error {
		defer s.release(target.ID)
		result, runErr := s.transport.Run(ctx, target.ID, action)
		if runErr != nil {
			report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "CONTAINER_ACTION_FAILED", Message: runErr.Error()}})
			return runErr
		}
		report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusSucceeded, Message: nonEmpty(strings.TrimSpace(result.Output), actionMessage(action))})
		return nil
	})
	if err != nil {
		s.release(target.ID)
		return jobs.Job{}, err
	}
	return job, nil
}

func (s *Service) release(id string) {
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
}

func validAction(action Action) bool {
	return action == ActionStart || action == ActionStop || action == ActionRestart || action == ActionRemove
}

func actionMessage(action Action) string {
	return map[Action]string{ActionStart: "容器已启动", ActionStop: "容器已停止", ActionRestart: "容器已重启", ActionRemove: "容器已移除"}[action]
}
