package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type Broker struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[int]chan struct{}
}

func NewBroker() *Broker {
	return &Broker{subscribers: make(map[int]chan struct{})}
}

func (b *Broker) Subscribe() (<-chan struct{}, func()) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	channel := make(chan struct{}, 1)
	b.subscribers[id] = channel
	b.mu.Unlock()
	return channel, func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
	}
}

func (b *Broker) Publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, subscriber := range b.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

type Service struct {
	store   *Store
	broker  *Broker
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewService(store *Store, broker *Broker) (*Service, error) {
	if broker == nil {
		broker = NewBroker()
	}
	service := &Service{store: store, broker: broker, cancels: make(map[string]context.CancelFunc)}
	events, err := store.RecoverInterrupted()
	if err != nil {
		return nil, fmt.Errorf("recover interrupted jobs: %w", err)
	}
	if len(events) > 0 {
		broker.Publish()
	}
	return service, nil
}

func (s *Service) Submit(kind, roomID, worldID string, targets []TargetSpec, runner Runner) (Job, error) {
	if runner == nil {
		return Job{}, errors.New("job runner is required")
	}
	return s.SubmitFactory(kind, roomID, worldID, targets, func(Job) Runner { return runner })
}

func (s *Service) SubmitFactory(kind, roomID, worldID string, targets []TargetSpec, factory func(Job) Runner) (Job, error) {
	if factory == nil {
		return Job{}, errors.New("job runner factory is required")
	}
	job, _, err := s.store.Create(kind, roomID, worldID, targets)
	if err != nil {
		return Job{}, err
	}
	runner := factory(job)
	if runner == nil {
		runner = func(context.Context, func(TargetResult)) error {
			return errors.New("job runner factory returned nil")
		}
	}
	s.broker.Publish()
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[job.ID] = cancel
	s.mu.Unlock()
	go s.run(ctx, job.ID, runner)
	return job, nil
}

func (s *Service) Cancel(jobID string) (Job, error) {
	job, _, err := s.store.RequestCancel(jobID)
	if err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	cancel := s.cancels[jobID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.broker.Publish()
	return job, nil
}

func (s *Service) Get(jobID string) (Job, error) { return s.store.Get(jobID) }

func (s *Service) List(filter ListFilter) ([]Job, int, error) { return s.store.List(filter) }

func (s *Service) EventsAfter(afterID int64, limit int) ([]Event, error) {
	return s.store.EventsAfter(afterID, limit)
}

func (s *Service) Subscribe() (<-chan struct{}, func()) { return s.broker.Subscribe() }

// Notify wakes the shared SSE stream for non-Job domain changes. Consumers
// refetch authoritative resources after receiving the refresh event.
func (s *Service) Notify() { s.broker.Publish() }

func (s *Service) run(ctx context.Context, jobID string, runner Runner) {
	defer func() {
		s.mu.Lock()
		cancel := s.cancels[jobID]
		delete(s.cancels, jobID)
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()
	if _, _, err := s.store.MarkRunning(jobID); err != nil {
		return
	}
	s.broker.Publish()
	report := func(result TargetResult) {
		if _, _, err := s.store.RecordTarget(jobID, result); err == nil {
			s.broker.Publish()
		}
	}
	runnerErr := runner(ctx, report)
	job, getErr := s.store.Get(jobID)
	if getErr != nil {
		return
	}
	for _, target := range job.Targets {
		if target.Status != StatusRunning {
			continue
		}
		result := TargetResult{TargetID: target.TargetID, Status: StatusFailed, Error: &Error{Code: "RUNNER_ABORTED", Message: "目标未返回执行结果"}}
		if ctx.Err() != nil {
			result.Status = StatusCanceled
			result.Error = &Error{Code: "JOB_CANCELED", Message: "任务已取消"}
		}
		if _, _, err := s.store.RecordTarget(jobID, result); err == nil {
			s.broker.Publish()
		}
	}
	if _, _, err := s.store.Complete(jobID, runnerErr, ctx.Err() != nil); err == nil {
		s.broker.Publish()
	}
}
