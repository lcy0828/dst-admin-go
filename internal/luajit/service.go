package luajit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
	Releases       []shared.LuaJITRelease           `json:"releases"`
	TargetID       string                           `json:"targetId"`
	TargetName     string                           `json:"targetName"`
	InstallationID string                           `json:"installationId"`
	OS             string                           `json:"os"`
	Arch           string                           `json:"arch"`
	Online         bool                             `json:"online"`
	CanInstall     bool                             `json:"canInstall"`
	Reason         string                           `json:"reason,omitempty"`
	Performance    *shared.RuntimePerformanceReport `json:"performance,omitempty"`
}
type Catalog struct {
	Transfers     []shared.LuaJITRelease `json:"transfers"`
	Installations []Installation         `json:"installations"`
}
type Service struct {
	runtimeStore *Store
	transfers    *Store
	targets      Targets
	jobs         *jobs.Service
	leases       *operationlease.Service
	mu           sync.Mutex
	busy         map[string]bool
}

func NewService(runtimeStore, transfers *Store, targets Targets, jobService *jobs.Service, leases *operationlease.Service) *Service {
	return &Service{runtimeStore: runtimeStore, transfers: transfers, targets: targets, jobs: jobService, leases: leases, busy: map[string]bool{}}
}
func (s *Service) Catalog(targetID string) (Catalog, error) {
	transfers, err := s.transfers.List()
	if err != nil {
		return Catalog{}, err
	}
	targets, err := s.targets.RuntimeTargets()
	if err != nil {
		return Catalog{}, err
	}
	result := Catalog{Transfers: transfers, Installations: []Installation{}}
	for _, target := range targets {
		if targetID != "" && target.ID != targetID {
			continue
		}
		for _, installation := range installations(target) {
			item := s.present(target, installation)
			if target.ID == "local" {
				item.Releases, err = s.runtimeStore.Available()
				if err != nil {
					return Catalog{}, err
				}
			}
			result.Installations = append(result.Installations, item)
		}
	}
	return result, nil
}
func installations(target agents.RuntimeTarget) []agents.RuntimeInstallation {
	if len(target.Installations) > 0 {
		return target.Installations
	}
	if !target.Configured {
		return nil
	}
	id := target.Config.InstallationID
	if id == "" {
		id = target.DefaultInstallationID
	}
	if id == "" {
		id = "default"
	}
	return []agents.RuntimeInstallation{{ID: id, Driver: "native", ServerPath: target.Config.ServerPath, ServerMode: target.Config.ServerMode, Performance: target.Performance}}
}
func (s *Service) present(target agents.RuntimeTarget, i agents.RuntimeInstallation) Installation {
	item := Installation{TargetID: target.ID, TargetName: target.Name, InstallationID: i.ID, OS: target.OS, Arch: target.Arch, Online: target.Online, Performance: i.Performance, CanInstall: true}
	switch {
	case !target.Online:
		item.Reason = "运行机器离线"
	case i.Driver == "container":
		item.Reason = "独立分片容器暂不支持在线安装，请使用 Native 或 All-in-One Runtime"
	case !OptionsFor(i, target).Supported():
		item.Reason = ErrUnsupported.Error()
	case target.ID != "local" && !hasCapability(target.Capabilities, "runtime.luajit.v2"):
		item.Reason = "请先升级该机器的 Agent，以支持 LuaJIT 安装"
	}
	item.CanInstall = item.Reason == ""
	if target.ID == "local" {
		value := OptionsFor(i, target).Inspect()
		item.Performance = &value
	}
	return item
}
func OptionsFor(i agents.RuntimeInstallation, t agents.RuntimeTarget) Options {
	return Options{ServerPath: i.ServerPath, ServerMode: i.ServerMode, Platform: t.OS, Architecture: t.Arch}
}
func hasCapability(values []string, key string) bool {
	for _, v := range values {
		if v == key {
			return true
		}
	}
	return false
}
func (s *Service) resolve(targetID, installationID string) (agents.RuntimeTarget, agents.RuntimeInstallation, error) {
	targets, err := s.targets.RuntimeTargets()
	if err != nil {
		return agents.RuntimeTarget{}, agents.RuntimeInstallation{}, err
	}
	for _, target := range targets {
		if target.ID == targetID {
			for _, i := range installations(target) {
				if i.ID == installationID {
					return target, i, nil
				}
			}
		}
	}
	return agents.RuntimeTarget{}, agents.RuntimeInstallation{}, agents.ErrRuntimeInstallationNotRegistered
}
func (s *Service) Inspect(ctx context.Context, targetID, installationID string, refresh ...bool) (Installation, error) {
	t, i, err := s.resolve(targetID, installationID)
	if err != nil {
		return Installation{}, err
	}
	item := s.present(t, i)
	if t.ID == "local" {
		if len(refresh) > 0 && refresh[0] {
			if err := s.runtimeStore.RefreshUpstream(ctx); err != nil {
				return item, err
			}
		}
		item.Releases, err = s.runtimeStore.Available()
		return item, err
	}
	if t.ID != "local" && t.Online && hasCapability(t.Capabilities, "runtime.luajit.v2") {
		request := nodeRequest(i.ID, shared.RuntimeActionLuaJITObserve)
		if len(refresh) > 0 && refresh[0] {
			request.LuaJIT = &shared.RuntimeLuaJITRequest{RefreshCatalog: true}
		}
		result, e := s.targets.ExecuteRuntime(ctx, t.ID, request, 30)
		if e != nil {
			return item, e
		}
		if result.Result.LuaJIT == nil {
			return item, errors.New("Agent 未返回 LuaJIT 检测结果")
		}
		item.Performance = result.Result.LuaJIT
		item.Releases = result.Result.LuaJITReleases
	}
	return item, nil
}
func nodeRequest(id string, action shared.RuntimeAction) shared.RuntimeOperationRequest {
	return shared.RuntimeOperationRequest{ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: uuid.NewString(), InstallationID: id, Action: action, Cluster: "LuaJITInstallation", Shard: "Runtime", TopologyRevision: "installation-v1"}
}

// Runtime is the default source. Controller transfer is always explicit.
func (s *Service) SubmitInstall(targetID, installationID, releaseID, source string) (jobs.Job, error) {
	if !packageID.MatchString(releaseID) {
		return jobs.Job{}, ErrInvalidPackage
	}
	if source == "" {
		source = "runtime"
	}
	payload := shared.RuntimeLuaJITRequest{ReleaseID: releaseID}
	switch source {
	case "runtime":
	case "controller":
		release, err := s.transfers.Get(releaseID)
		if err != nil {
			return jobs.Job{}, err
		}
		payload = shared.RuntimeLuaJITRequest{Release: &release}
	default:
		return jobs.Job{}, errors.New("LuaJIT 安装来源无效")
	}
	return s.submit(targetID, installationID, shared.RuntimeActionLuaJITInstall, payload)
}
func (s *Service) ImportURL(targetID, installationID, rawURL, digest string) (jobs.Job, error) {
	rawURL, digest = strings.TrimSpace(rawURL), strings.ToLower(strings.TrimSpace(digest))
	if err := ValidateSource(rawURL, digest); err != nil {
		return jobs.Job{}, err
	}
	return s.submit(targetID, installationID, shared.RuntimeActionLuaJITDownload, shared.RuntimeLuaJITRequest{SourceURL: rawURL, SHA256: digest})
}
func (s *Service) submit(targetID, installationID string, action shared.RuntimeAction, payload shared.RuntimeLuaJITRequest) (jobs.Job, error) {
	t, i, err := s.resolve(targetID, installationID)
	if err != nil {
		return jobs.Job{}, err
	}
	if item := s.present(t, i); !item.CanInstall {
		return jobs.Job{}, errors.New(item.Reason)
	}
	key := targetID + "/" + installationID
	s.mu.Lock()
	if s.busy[key] {
		s.mu.Unlock()
		return jobs.Job{}, ErrBusy
	}
	s.busy[key] = true
	s.mu.Unlock()
	clear := func() { s.mu.Lock(); delete(s.busy, key); s.mu.Unlock() }
	kind, message := "luajit.install", "LuaJIT 安装完成"
	if action == shared.RuntimeActionLuaJITDownload {
		kind, message = "luajit.download", "运行节点上的 LuaJIT 安装包已就绪"
	}
	job, err := s.jobs.SubmitFactory(kind, "", "", []jobs.TargetSpec{{ID: key, Name: t.Name + " / " + i.ID}}, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			defer clear()
			ctx, cancel := context.WithTimeout(ctx, 9*time.Minute)
			defer cancel()
			ctx = operationprogress.WithReporter(ctx, func(p operationprogress.Update) {
				_, _ = s.jobs.UpdateProgressDetail(job.ID, jobs.ProgressUpdate{Progress: p.Percent, Message: p.Message, CurrentBytes: p.CurrentBytes, TotalBytes: p.TotalBytes, Detail: &jobs.ProgressDetail{Stage: p.Stage, TargetID: targetID, InstallationID: installationID}})
			})
			emit(ctx, "prepare", 2, "正在准备运行节点上的 LuaJIT 任务")
			t, i, err := s.resolve(targetID, installationID)
			if err == nil {
				if item := s.present(t, i); !item.CanInstall {
					err = errors.New(item.Reason)
				}
			}
			if err == nil {
				err = s.execute(ctx, t, i, action, payload, job.ID)
			}
			outcome := jobs.TargetResult{TargetID: key, Status: jobs.StatusSucceeded, Message: message}
			if err != nil {
				outcome.Status, outcome.Message = jobs.StatusFailed, ""
				code := "LUAJIT_INSTALL_FAILED"
				if action == shared.RuntimeActionLuaJITDownload {
					code = "LUAJIT_DOWNLOAD_FAILED"
				}
				outcome.Error = &jobs.Error{Code: code, Message: err.Error()}
			}
			report(outcome)
			return nil
		}
	})
	if err != nil {
		clear()
	}
	return job, err
}
func (s *Service) execute(ctx context.Context, t agents.RuntimeTarget, i agents.RuntimeInstallation, action shared.RuntimeAction, payload shared.RuntimeLuaJITRequest, jobID string) error {
	lease, err := s.leases.Acquire(ctx, "luajit:"+t.ID+":"+i.ID, jobID, 10*time.Minute)
	if err != nil {
		return err
	}
	defer s.leases.Release(lease)
	if t.ID == "local" {
		return s.executeLocal(ctx, t, i, action, payload)
	}
	// No controller package lookup/grant for the normal Runtime-owned path.
	if payload.Release != nil {
		token, err := s.transfers.Grant(payload.Release.ID)
		if err != nil {
			return err
		}
		defer s.transfers.Revoke(token)
		payload.DownloadPath, payload.DownloadToken = "/luajit-packages/"+payload.Release.ID, token
	}
	request := nodeRequest(i.ID, action)
	request.OperationKey, request.LeaseID, request.FencingToken = jobID, lease.LeaseID, lease.FencingToken
	request.LeaseExpiresAt, request.LuaJIT = &lease.ExpiresAt, &payload
	result, err := s.targets.ExecuteRuntime(ctx, t.ID, request, 540)
	if err != nil {
		return err
	}
	r := result.Result
	if r.Outcome != shared.RuntimeOutcomeConfirmed || r.LuaJITRelease == nil {
		return fmt.Errorf("Agent 未确认 LuaJIT 操作成功: %s", r.Message)
	}
	expectedID := payload.ReleaseID
	if payload.Release != nil {
		expectedID = payload.Release.ID
	}
	if action == shared.RuntimeActionLuaJITDownload {
		expectedID = payload.SHA256
	}
	if r.LuaJITRelease.ID != expectedID || r.LuaJITRelease.SHA256 != expectedID {
		return errors.New("Agent 返回的 LuaJIT 安装包与请求不一致")
	}
	if action == shared.RuntimeActionLuaJITInstall && (r.LuaJIT == nil || !r.LuaJIT.CanEnable || r.LuaJIT.PackageVersion != r.LuaJITRelease.Version) {
		return fmt.Errorf("Agent 未确认 LuaJIT 安装成功: %s", r.Message)
	}
	emit(ctx, "done", 100, r.Message)
	return nil
}
func (s *Service) executeLocal(ctx context.Context, t agents.RuntimeTarget, i agents.RuntimeInstallation, action shared.RuntimeAction, payload shared.RuntimeLuaJITRequest) error {
	if action == shared.RuntimeActionLuaJITDownload {
		_, err := s.runtimeStore.ImportURL(ctx, payload.SourceURL, payload.SHA256)
		if err == nil {
			emit(ctx, "done", 100, "运行节点上的 LuaJIT 安装包已就绪")
		}
		return err
	}
	if err := OptionsFor(i, t).CheckStopped(ctx); err != nil {
		return err
	}
	var release shared.LuaJITRelease
	var err error
	if payload.Release != nil {
		path, e := s.transfers.Path(payload.Release.ID)
		if e != nil {
			return e
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		defer f.Close()
		release, err = s.runtimeStore.Save(ctx, f, payload.Release.SHA256)
	} else {
		release, err = s.runtimeStore.EnsureAvailable(ctx, payload.ReleaseID)
	}
	if err != nil {
		return err
	}
	archive, err := s.runtimeStore.Path(release.ID)
	if err != nil {
		return err
	}
	_, err = Install(ctx, OptionsFor(i, t), release, archive)
	return err
}
