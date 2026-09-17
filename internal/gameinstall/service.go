package gameinstall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/operationlease"
	"dont/internal/operationprogress"
	"dont/shared"
	"github.com/google/uuid"
)

type Targets interface {
	RuntimeTargets() ([]agents.RuntimeTarget, error)
	ExecuteRuntime(context.Context, string, shared.RuntimeOperationRequest, int) (agents.RuntimeExecutionResult, error)
}
type Installation struct {
	TargetID       string `json:"targetId"`
	TargetName     string `json:"targetName"`
	InstallationID string `json:"installationId"`
	Online         bool   `json:"online"`
	OS             string `json:"os"`
	Supported      bool   `json:"supported"`
	Error          string `json:"error,omitempty"`
	shared.GameInstallationReport
}
type Service struct {
	targets Targets
	jobs    *jobs.Service
	leases  *operationlease.Service
	mu      sync.Mutex
	busy    map[string]bool
}

func NewService(t Targets, j *jobs.Service, l *operationlease.Service) *Service {
	return &Service{targets: t, jobs: j, leases: l, busy: map[string]bool{}}
}
func supported(t agents.RuntimeTarget) bool {
	if t.ID == "local" {
		return true
	}
	for _, v := range t.Capabilities {
		if v == "runtime.game-install.v1" {
			return true
		}
	}
	return false
}
func options(t agents.RuntimeTarget, i agents.RuntimeInstallation) Options {
	return Options{ServerPath: i.ServerPath, SavePath: i.SavePath, SteamCMDPath: i.SteamCMDPath, ServerMode: i.ServerMode, Driver: i.Driver, Platform: t.OS, Architecture: t.Arch}
}
func installations(t agents.RuntimeTarget) []agents.RuntimeInstallation {
	if len(t.Installations) > 0 {
		return t.Installations
	}
	if !t.Configured {
		return nil
	}
	id := t.Config.InstallationID
	if id == "" {
		id = "default"
	}
	return []agents.RuntimeInstallation{{ID: id, Driver: "native", ServerPath: t.Config.ServerPath, SavePath: t.Config.SavePath, SteamCMDPath: t.Config.SteamCMDPath, ServerMode: t.Config.ServerMode}}
}
func (s *Service) resolve(targetID, installationID string) (agents.RuntimeTarget, agents.RuntimeInstallation, error) {
	targets, err := s.targets.RuntimeTargets()
	if err != nil {
		return agents.RuntimeTarget{}, agents.RuntimeInstallation{}, err
	}
	for _, t := range targets {
		if t.ID == targetID {
			for _, i := range installations(t) {
				if i.ID == installationID {
					return t, i, nil
				}
			}
		}
	}
	return agents.RuntimeTarget{}, agents.RuntimeInstallation{}, agents.ErrRuntimeInstallationNotRegistered
}
func request(i agents.RuntimeInstallation, action shared.RuntimeAction, p shared.GameInstallationRequest) shared.RuntimeOperationRequest {
	return shared.RuntimeOperationRequest{ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: uuid.NewString(), InstallationID: i.ID, Action: action, Cluster: "GameInstallation", Shard: "Runtime", TopologyRevision: "installation-v1", GameInstallation: &p}
}
func (s *Service) observe(ctx context.Context, t agents.RuntimeTarget, i agents.RuntimeInstallation, path string) (shared.GameInstallationReport, error) {
	if !t.Online {
		return shared.GameInstallationReport{}, agents.ErrAgentOffline
	}
	if !supported(t) {
		return shared.GameInstallationReport{}, errors.New("请升级该机器的 Agent 以支持游戏安装管理")
	}
	if t.ID == "local" {
		o := options(t, i)
		if path != "" {
			return o.Probe(path)
		}
		return o.Inspect(), nil
	}
	r, err := s.targets.ExecuteRuntime(ctx, t.ID, request(i, shared.RuntimeActionGameInstallationObserve, shared.GameInstallationRequest{Path: path}), 30)
	if err != nil {
		return shared.GameInstallationReport{}, err
	}
	if r.Result.GameInstallation == nil {
		return shared.GameInstallationReport{}, errors.New("Agent 未返回安装状态")
	}
	return *r.Result.GameInstallation, nil
}

// Catalog includes registered installations with no room placements and nodes
// whose installation registry is empty. It performs no Steam/network discovery.
func (s *Service) Catalog(ctx context.Context, targetID string) ([]Installation, error) {
	targets, err := s.targets.RuntimeTargets()
	if err != nil {
		return nil, err
	}
	values := []Installation{}
	type route struct {
		t agents.RuntimeTarget
		i agents.RuntimeInstallation
	}
	routes := []route{}
	for _, t := range targets {
		if targetID != "" && t.ID != targetID {
			continue
		}
		is := installations(t)
		if len(is) == 0 {
			values = append(values, Installation{TargetID: t.ID, TargetName: t.Name, Online: t.Online, OS: t.OS, Error: "该机器尚未登记游戏安装位置"})
			routes = append(routes, route{t: t})
			continue
		}
		for _, i := range is {
			values = append(values, Installation{TargetID: t.ID, TargetName: t.Name, InstallationID: i.ID, Online: t.Online, OS: t.OS, Supported: supported(t), GameInstallationReport: shared.GameInstallationReport{ServerPath: i.ServerPath, SavePath: i.SavePath}})
			routes = append(routes, route{t, i})
		}
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, 2)
	for n := range values {
		if routes[n].i.ID == "" {
			continue
		}
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				values[n].Error = ctx.Err().Error()
				return
			}
			read, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			r, e := s.observe(read, routes[n].t, routes[n].i, "")
			if e != nil {
				values[n].Error = e.Error()
				return
			}
			values[n].GameInstallationReport = r
		}(n)
	}
	wg.Wait()
	return values, ctx.Err()
}
func (s *Service) Probe(ctx context.Context, targetID, installationID, path string) (shared.GameInstallationReport, error) {
	t, i, err := s.resolve(targetID, installationID)
	if err != nil {
		return shared.GameInstallationReport{}, err
	}
	return s.observe(ctx, t, i, path)
}
func (s *Service) Submit(targetID, installationID string, adopt bool, p shared.GameInstallationRequest) (jobs.Job, error) {
	t, i, err := s.resolve(targetID, installationID)
	if err != nil {
		return jobs.Job{}, err
	}
	if !t.Online {
		return jobs.Job{}, agents.ErrAgentOffline
	}
	if !supported(t) {
		return jobs.Job{}, agents.ErrUnsupportedAction
	}
	if adopt {
		if p.Path == "" || len(p.Fingerprint) != 64 {
			return jobs.Job{}, errors.New("请先检测已有游戏目录")
		}
	} else if p.Path != "" || p.Fingerprint != "" {
		return jobs.Job{}, errors.New("下载只能使用已登记的安装位置")
	}
	key := t.ID + "/" + i.ID
	s.mu.Lock()
	if s.busy[key] {
		s.mu.Unlock()
		return jobs.Job{}, errors.New("该安装正在执行任务")
	}
	s.busy[key] = true
	s.mu.Unlock()
	clear := func() { s.mu.Lock(); delete(s.busy, key); s.mu.Unlock() }
	action := shared.RuntimeActionGameInstallationInstall
	kind := "game.install"
	if adopt {
		action = shared.RuntimeActionGameInstallationAdopt
		kind = "game.adopt"
	}
	job, err := s.jobs.SubmitFactory(kind, "", "", []jobs.TargetSpec{{ID: key, Name: t.Name + " / " + i.ID}}, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			defer clear()
			ctx, cancel := context.WithTimeout(ctx, 29*time.Minute)
			defer cancel()
			ctx = operationprogress.WithReporter(ctx, func(p operationprogress.Update) {
				observedAt := time.Now().UTC()
				_, _ = s.jobs.UpdateProgressDetail(job.ID, jobs.ProgressUpdate{
					Progress: min(99, p.Percent), Message: p.Message,
					CurrentBytes: p.CurrentBytes, TotalBytes: p.TotalBytes, BytesPerSecond: p.BytesPerSecond,
					Detail: &jobs.ProgressDetail{Stage: p.Stage, TargetID: t.ID, InstallationID: i.ID, ObservedAt: &observedAt},
				})
			})
			e := s.execute(ctx, t.ID, i.ID, action, p, job.ID)
			r := jobs.TargetResult{TargetID: key, Status: jobs.StatusSucceeded, Message: "游戏服务端已就绪"}
			if e != nil {
				r.Status = jobs.StatusFailed
				r.Message = ""
				r.Error = &jobs.Error{Code: "GAME_INSTALLATION_FAILED", Message: e.Error()}
			}
			report(r)
			return nil
		}
	})
	if err != nil {
		clear()
	}
	return job, err
}
func (s *Service) execute(ctx context.Context, targetID, installationID string, action shared.RuntimeAction, p shared.GameInstallationRequest, jobID string) error {
	t, i, err := s.resolve(targetID, installationID)
	if err != nil {
		return err
	}
	if !t.Online {
		return agents.ErrAgentOffline
	}
	lease, err := s.leases.Acquire(ctx, "game-install:"+targetID+":"+installationID, jobID, 10*time.Minute)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, e := s.leases.Renew(ctx, lease, 10*time.Minute); e != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(done); <-renewed; _ = s.leases.Release(lease) }()
	if t.ID == "local" {
		o := options(t, i)
		if action == shared.RuntimeActionGameInstallationAdopt {
			_, err = o.Adopt(ctx, p.Path, p.Fingerprint)
		} else {
			_, err = o.Install(ctx)
		}
		return err
	}
	r := request(i, action, p)
	r.OperationKey = jobID
	r.LeaseID = lease.LeaseID
	r.FencingToken = lease.FencingToken
	r.LeaseExpiresAt = &lease.ExpiresAt
	timeout := 30
	if action == shared.RuntimeActionGameInstallationInstall {
		timeout = 1740
	}
	result, err := s.targets.ExecuteRuntime(ctx, t.ID, r, timeout)
	if err != nil {
		return err
	}
	if result.Result.Outcome != shared.RuntimeOutcomeConfirmed || result.Result.GameInstallation == nil || !result.Result.GameInstallation.Installed {
		return fmt.Errorf("Agent 未确认游戏安装成功: %s", result.Result.Message)
	}
	return nil
}
