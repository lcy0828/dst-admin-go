package backups

import (
	"context"
	"fmt"
	"sync"
	"time"

	"dont/internal/jobs"
)

type Scheduler struct {
	backups  *Service
	jobs     *jobs.Service
	interval time.Duration
	mu       sync.Mutex
	inFlight map[string]bool
}

func NewScheduler(backupService *Service, jobService *jobs.Service) *Scheduler {
	return &Scheduler{backups: backupService, jobs: jobService, interval: time.Minute, inFlight: make(map[string]bool)}
}

func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		_ = s.RunDue()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = s.RunDue()
			}
		}
	}()
}

func (s *Scheduler) RunDue() error {
	policies, err := s.backups.DuePolicies()
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if !s.begin(policy.RoomID) {
			continue
		}
		policy := policy
		targets := []jobs.TargetSpec{{ID: policy.RoomID, Name: "自动快照"}}
		job, submitErr := s.jobs.SubmitFactory("backup.snapshot", policy.RoomID, "", targets, func(job jobs.Job) jobs.Runner {
			return func(ctx context.Context, report func(jobs.TargetResult)) error {
				defer s.end(policy.RoomID)
				value, createErr := s.backups.Create(ctx, policy.RoomID, "", KindSnapshot, job.ID)
				if createErr == nil {
					_, _, createErr = s.backups.PruneSnapshots(policy.RoomID, policy.MaxSnapshots)
				}
				_ = s.backups.MarkPolicyRun(policy.RoomID, job.ID, createErr)
				if createErr != nil {
					report(jobs.TargetResult{TargetID: policy.RoomID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "SNAPSHOT_FAILED", Message: createErr.Error()}})
					return nil
				}
				report(jobs.TargetResult{TargetID: policy.RoomID, Status: jobs.StatusSucceeded, Message: "自动快照已创建：" + value.Name})
				return nil
			}
		})
		if submitErr != nil {
			s.end(policy.RoomID)
			_ = s.backups.MarkPolicyRun(policy.RoomID, "", submitErr)
			return fmt.Errorf("submit snapshot for room %s: %w", policy.RoomID, submitErr)
		}
		_ = job
	}
	return nil
}

func (s *Scheduler) begin(roomID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[roomID] {
		return false
	}
	s.inFlight[roomID] = true
	return true
}

func (s *Scheduler) end(roomID string) {
	s.mu.Lock()
	delete(s.inFlight, roomID)
	s.mu.Unlock()
}
