package modupdates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"dont/internal/jobs"
	"dont/internal/maintenance"
	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/internal/players"
	"dont/internal/rooms"
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
}

type Catalog interface {
	CheckUpdates(context.Context, string) (mods.ActionResult, error)
}

type InstallationRoomCatalog interface {
	InstallationRoomIDs(context.Context, string, string) ([]string, error)
}

type ContentUpdater interface {
	UpdateRoomMods(context.Context, string, []string, io.Writer) (mods.ActionResult, error)
}

type Restarter interface {
	RestartRunningWorlds(context.Context, string, string) error
}

type Presence interface {
	RefreshPresence(context.Context, string) (players.PresenceSnapshot, error)
}

type Announcer interface {
	AnnounceModUpdate(context.Context, string, string, string) error
}

type AnnounceFunc func(context.Context, string, string, string) error

func (f AnnounceFunc) AnnounceModUpdate(ctx context.Context, roomID, message, jobID string) error {
	return f(ctx, roomID, message, jobID)
}

type Service struct {
	store     *Store
	rooms     RoomCatalog
	catalog   Catalog
	content   ContentUpdater
	restarter Restarter
	presence  Presence
	jobs      *jobs.Service
	announcer Announcer
	online    maintenance.OnlineCounter
	now       func() time.Time
	wake      chan struct{}
	activeMu  sync.Mutex
	active    map[string]string
}

func NewService(store *Store, roomCatalog RoomCatalog, catalog Catalog, content ContentUpdater, restarter Restarter, presence Presence, jobService *jobs.Service) (*Service, error) {
	if store == nil || roomCatalog == nil || catalog == nil || content == nil || restarter == nil || presence == nil || jobService == nil {
		return nil, errors.New("Mod update service dependencies are required")
	}
	return &Service{
		store: store, rooms: roomCatalog, catalog: catalog, content: content, restarter: restarter, presence: presence, jobs: jobService,
		now: time.Now, wake: make(chan struct{}, 1), active: make(map[string]string),
	}, nil
}

func (s *Service) ConfigureAnnouncer(announcer Announcer) { s.announcer = announcer }

func (s *Service) RefreshInstallation(ctx context.Context, targetID, installationID string) error {
	roomsByInstallation, ok := s.catalog.(InstallationRoomCatalog)
	if !ok {
		return errors.New("Mod update catalog cannot resolve installation rooms")
	}
	roomIDs, err := roomsByInstallation.InstallationRoomIDs(ctx, targetID, installationID)
	if err != nil {
		return err
	}
	var failures []error
	for _, roomID := range normalizedIDs(roomIDs) {
		if _, err := s.SubmitCheck(roomID); err != nil && !errors.Is(err, ErrBusy) {
			failures = append(failures, fmt.Errorf("start room %s Mod update check: %w", roomID, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) Overview(roomID string) (Overview, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return Overview{}, err
	}
	policy, err := s.store.Policy(roomID)
	if err != nil {
		return Overview{}, err
	}
	state, err := s.store.State(roomID)
	return Overview{Policy: policy, State: state}, err
}

func (s *Service) UpdatePolicy(roomID string, input PolicyInput) (Overview, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return Overview{}, err
	}
	if err := normalizePolicyInput(&input); err != nil {
		return Overview{}, err
	}
	previous, err := s.store.Policy(roomID)
	if err != nil {
		return Overview{}, err
	}
	policy, err := s.store.SavePolicy(roomID, input)
	if err != nil {
		return Overview{}, err
	}
	state, err := s.store.State(roomID)
	if err != nil {
		return Overview{}, err
	}
	now := s.now().UTC()
	if policy.AutoCheck {
		if !previous.AutoCheck || state.NextCheckAt == nil || previous.CheckIntervalMinutes != policy.CheckIntervalMinutes {
			state.NextCheckAt = &now
		}
		if policy.AutoPrepare && policy.ApplyWhenEmpty && pendingApplyStatus(state.Status) && state.PreparedAt != nil {
			state.NextActionAt = &now
		}
	} else {
		state.NextCheckAt, state.NextActionAt = nil, nil
		if state.Status == StatusWaitingForPlayers || state.Status == StatusScheduled {
			state.Status = StatusPrepared
		}
	}
	if !policy.AutoPrepare || !policy.ApplyWhenEmpty {
		state.NextActionAt = nil
	}
	state, err = s.saveState(state)
	if err != nil {
		return Overview{}, err
	}
	s.signal()
	return Overview{Policy: policy, State: state}, nil
}

func (s *Service) SubmitCheck(roomID string) (jobs.Job, error) {
	return s.submit(roomID, DueCheckOnly)
}

func (s *Service) SubmitApplyWhenEmpty(roomID string) (jobs.Job, error) {
	state, err := s.store.State(roomID)
	if err != nil {
		return jobs.Job{}, err
	}
	if state.PreparedAt == nil || !pendingApplyStatus(state.Status) {
		return jobs.Job{}, ErrInvalidInput
	}
	return s.submit(roomID, DueApply)
}

func (s *Service) SubmitApplyNow(roomID string) (jobs.Job, error) {
	return s.submit(roomID, DueApplyNow)
}

func (s *Service) submit(roomID string, action DueAction) (jobs.Job, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return jobs.Job{}, err
	}
	s.activeMu.Lock()
	if id := s.active[roomID]; id != "" {
		s.activeMu.Unlock()
		if id != "submitting" {
			if current, err := s.jobs.Get(id); err == nil {
				return current, nil
			}
		}
		return jobs.Job{}, ErrBusy
	}
	s.active[roomID] = "submitting"
	s.activeMu.Unlock()
	kind, label := "mod.update.check", "检查房间模组更新"
	if action == DueApply {
		kind, label = "mod.update.activate", "等待无人并应用模组更新"
	} else if action == DueApplyNow {
		kind, label = "mod.update.activate", "更新模组并重启运行中的世界"
	}
	target := jobs.TargetSpec{ID: updateJobTarget(roomID), Name: label}
	job, err := s.jobs.SubmitFactory(kind, roomID, "", []jobs.TargetSpec{target}, func(job jobs.Job) jobs.Runner {
		s.activeMu.Lock()
		s.active[roomID] = job.ID
		s.activeMu.Unlock()
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			defer s.clearActive(roomID, job.ID)
			ctx = operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
				progress := update.Percent
				if progress <= 0 {
					progress = 10
				}
				_, _ = s.jobs.UpdateProgressDetail(job.ID, jobs.ProgressUpdate{
					Progress: progress, Message: update.Message,
					Detail:       &jobs.ProgressDetail{Stage: update.Stage, WorkshopID: update.WorkshopID, CurrentItem: update.CurrentItem, TotalItems: update.TotalItems, TargetID: update.TargetID, InstallationID: update.InstallationID, Items: update.Items, Worlds: update.Worlds},
					CurrentBytes: update.CurrentBytes, TotalBytes: update.TotalBytes, BytesPerSecond: update.BytesPerSecond,
				})
			})
			var state State
			var runErr error
			switch action {
			case DueApply:
				state, runErr = s.applyWhenEmpty(ctx, roomID, job.ID)
			case DueApplyNow:
				state, runErr = s.applyNow(ctx, roomID, job.ID)
			default:
				state, runErr = s.check(ctx, roomID, job.ID, action == DueCheck)
			}
			if runErr != nil {
				code := state.ErrorCode
				if code == "" {
					code = updateErrorCode(runErr)
				}
				report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: code, Message: runErr.Error()}})
				return nil
			}
			report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusSucceeded, Message: "模组更新状态已刷新"})
			return nil
		}
	})
	if err != nil {
		s.clearActive(roomID, "submitting")
		return jobs.Job{}, err
	}
	return job, nil
}

func (s *Service) check(ctx context.Context, roomID, jobID string, automatic bool) (State, error) {
	policy, err := s.store.Policy(roomID)
	if err != nil {
		return State{}, err
	}
	state, err := s.store.State(roomID)
	if err != nil {
		return State{}, err
	}
	now := s.now().UTC()
	state.Status, state.ErrorCode, state.ErrorMessage = StatusChecking, "", ""
	state.NextActionAt = nil
	state.NextCheckAt = timePointer(now.Add(time.Duration(policy.CheckIntervalMinutes) * time.Minute))
	if state, err = s.saveState(state); err != nil {
		return state, err
	}
	operationprogress.Report(ctx, operationprogress.Update{Percent: 5, Message: "正在检查 Workshop 更新"})
	checked, err := s.catalog.CheckUpdates(ctx, roomID)
	if err != nil {
		state.AvailableModIDs = normalizedIDs(append(state.AvailableModIDs, checked.ModIDs...))
		return s.fail(state, "MOD_UPDATE_CHECK_FAILED", err, nil)
	}
	state = s.recordCheck(state, checked.ModIDs)
	if state.PreparedAt != nil && policy.AutoCheck && policy.AutoPrepare && policy.ApplyWhenEmpty {
		state.NextActionAt = timePointer(s.now())
	}
	if state, err = s.saveState(state); err != nil {
		return state, err
	}
	// Manual/notify-only checks never download content or restart worlds.
	policy, err = s.store.Policy(roomID)
	if err != nil {
		return state, err
	}
	if !automatic || !policy.AutoCheck || !policy.AutoPrepare {
		return state, nil
	}
	if len(checked.ModIDs) == 0 && state.PreparedAt == nil {
		return state, nil
	}
	ctx, release, affected, err := s.lockAffectedRooms(ctx, roomID)
	if err != nil {
		return s.fail(state, "MAINTENANCE_SCOPE_FAILED", err, nil)
	}
	defer release()
	// Unattended downloads and restarts must both wait for the shared rooms.
	presence, presenceErr := s.refreshAffectedPresence(ctx, affected)
	state.OnlinePlayers, state.StaleOnlinePlayers, state.PresenceFresh = presence.Online, presence.StaleOnline, presence.Fresh
	if presenceErr != nil || !presence.Fresh || presence.Online > 0 {
		state.EmptySince, state.NextActionAt = nil, nil
		state.NextCheckAt = timePointer(s.now().Add(time.Minute))
		if presenceErr != nil {
			return s.fail(state, "PLAYER_PRESENCE_CHECK_FAILED", presenceErr, nil)
		}
		if !presence.Fresh {
			return s.fail(state, "PLAYER_PRESENCE_STALE", maintenance.ErrPresenceUnavailable, nil)
		}
		state.Status = StatusWaitingForPlayers
		if policy.GameAnnouncement && state.AnnouncementPlanHash == "" && s.announcer != nil {
			if s.announcer.AnnounceModUpdate(ctx, roomID, "检测到模组更新，将在所有受影响房间无人时更新并重启。", jobID) == nil {
				state.AnnouncementPlanHash = "waiting"
			}
		}
		return s.saveState(state)
	}
	if len(checked.ModIDs) > 0 {
		state, err = s.downloadUpdates(ctx, state, checked.ModIDs)
		if err != nil {
			return state, err
		}
	}
	if state.PreparedAt == nil || !policy.ApplyWhenEmpty {
		return state, nil
	}
	return s.applyWhenEmpty(ctx, roomID, jobID)
}

// A successful node download changes the disk version before a waiting restart.
// Subsequent checks must retain that pending operation even when disks are current.
// Old plan hashes only described Controller cache preparation, not node downloads.
func (s *Service) recordCheck(state State, ids []string) State {
	state.LastCheckedAt = timePointer(s.now())
	if state.PreparedAt != nil && state.PreparedPlanHash == "" {
		state.AvailableModIDs = normalizedIDs(append(state.AvailableModIDs, ids...))
		state.Status = StatusPrepared
	} else {
		state.AvailableModIDs = normalizedIDs(ids)
		state.PreparedAt, state.EmptySince = nil, nil
		state.Status = StatusAvailable
		if len(state.AvailableModIDs) == 0 {
			state.Status = StatusIdle
			state.AnnouncementPlanHash = ""
		}
	}
	state.PreparedPlanHash, state.PublicationID = "", ""
	state.ErrorCode, state.ErrorMessage = "", ""
	return state
}

func (s *Service) downloadUpdates(ctx context.Context, state State, ids []string) (State, error) {
	state.Status, state.NextActionAt, state.EmptySince = StatusPreparing, nil, nil
	var err error
	if state, err = s.saveState(state); err != nil {
		return state, err
	}
	operationprogress.Report(ctx, operationprogress.Update{Percent: 10, Message: "正在运行机器上直接更新房间模组"})
	downloadCtx := operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		update.Percent = 10 + min(100, max(0, update.Percent))/2
		operationprogress.Report(ctx, update)
	})
	result, err := s.content.UpdateRoomMods(downloadCtx, state.RoomID, ids, io.Discard)
	if err != nil {
		return s.fail(state, "MOD_RUNTIME_UPDATE_FAILED", err, nil)
	}
	// The user may have disabled every requested Mod since the check.
	if len(result.ModIDs) == 0 && state.PreparedAt == nil {
		state.Status, state.AvailableModIDs = StatusIdle, []string{}
	} else {
		state.Status, state.PreparedAt = StatusPrepared, timePointer(s.now())
	}
	state.PreparedPlanHash, state.PublicationID = "", ""
	state.ErrorCode, state.ErrorMessage = "", ""
	return s.saveState(state)
}

func (s *Service) applyNow(ctx context.Context, roomID, jobID string) (State, error) {
	state, err := s.store.State(roomID)
	if err != nil {
		return State{}, err
	}
	state.Status, state.ErrorCode, state.ErrorMessage = StatusChecking, "", ""
	state.NextActionAt, state.EmptySince = nil, nil
	if state, err = s.saveState(state); err != nil {
		return state, err
	}
	operationprogress.Report(ctx, operationprogress.Update{Percent: 5, Message: "正在重新检查 Workshop 更新"})
	checked, err := s.catalog.CheckUpdates(ctx, roomID)
	if err != nil {
		state.AvailableModIDs = normalizedIDs(append(state.AvailableModIDs, checked.ModIDs...))
		return s.fail(state, "MOD_UPDATE_CHECK_FAILED", err, nil)
	}
	state = s.recordCheck(state, checked.ModIDs)
	if state, err = s.saveState(state); err != nil {
		return state, err
	}
	if len(checked.ModIDs) > 0 {
		state, err = s.downloadUpdates(ctx, state, checked.ModIDs)
		if err != nil {
			return state, err
		}
	}
	if state.PreparedAt == nil {
		operationprogress.Report(ctx, operationprogress.Update{Percent: 100, Message: "没有需要应用的房间模组更新"})
		return state, nil
	}
	return s.restartPrepared(ctx, roomID, jobID, state)
}

func (s *Service) applyWhenEmpty(ctx context.Context, roomID, jobID string) (State, error) {
	policy, err := s.store.Policy(roomID)
	if err != nil {
		return State{}, err
	}
	state, err := s.store.State(roomID)
	if err != nil {
		return State{}, err
	}
	if !policy.AutoCheck || !policy.AutoPrepare || !policy.ApplyWhenEmpty || policy.RestartWithPlayers || state.PreparedAt == nil {
		return state, ErrInvalidInput
	}
	if state.PreparedPlanHash != "" {
		return s.check(ctx, roomID, jobID, true)
	}
	ctx, release, affected, err := s.lockAffectedRooms(ctx, roomID)
	if err != nil {
		return s.fail(state, "MAINTENANCE_SCOPE_FAILED", err, nil)
	}
	defer release()
	now := s.now().UTC()
	state.NextActionAt = timePointer(now.Add(time.Minute))
	state, err = s.saveState(state)
	if err != nil {
		return state, err
	}
	operationprogress.Report(ctx, operationprogress.Update{Percent: 65, Message: "正在主动确认所有世界的在线玩家"})
	presence, presenceErr := s.refreshAffectedPresence(ctx, affected)
	if presenceErr != nil {
		state.EmptySince = nil
		return s.fail(state, "PLAYER_PRESENCE_CHECK_FAILED", presenceErr, timePointer(now.Add(time.Minute)))
	}
	state.OnlinePlayers, state.StaleOnlinePlayers, state.PresenceFresh = presence.Online, presence.StaleOnline, presence.Fresh
	if !presence.Fresh {
		state.EmptySince = nil
		return s.fail(state, "PLAYER_PRESENCE_STALE", errors.New("玩家遥测不完整，已阻止自动重启"), timePointer(now.Add(time.Minute)))
	}
	if presence.Online > 0 {
		state.Status, state.EmptySince = StatusWaitingForPlayers, nil
		state.ErrorCode, state.ErrorMessage = "", ""
		state.NextActionAt = timePointer(now.Add(time.Minute))
		preparedKey := state.PreparedAt.Format(time.RFC3339Nano)
		if policy.GameAnnouncement && state.AnnouncementPlanHash != preparedKey && s.announcer != nil {
			if announceErr := s.announcer.AnnounceModUpdate(ctx, roomID, "服务器模组更新已准备完成，将在所有玩家离线后自动应用。", jobID); announceErr == nil {
				state.AnnouncementPlanHash = preparedKey
			}
		}
		return s.saveState(state)
	}
	if state.EmptySince == nil {
		state.Status = StatusScheduled
		state.EmptySince = &now
		state.NextActionAt = timePointer(now.Add(time.Duration(policy.EmptyGraceSeconds) * time.Second))
		state.ErrorCode, state.ErrorMessage = "", ""
		return s.saveState(state)
	}
	deadline := state.EmptySince.Add(time.Duration(policy.EmptyGraceSeconds) * time.Second)
	if deadline.After(now) {
		state.Status, state.NextActionAt = StatusScheduled, &deadline
		return s.saveState(state)
	}
	// Check versions again after waiting; a newer release must be downloaded
	// before restarting. Download time does not count as empty-player grace.
	operationprogress.Report(ctx, operationprogress.Update{Percent: 70, Message: "正在确认等待期间是否又有 Workshop 更新"})
	checked, err := s.catalog.CheckUpdates(ctx, roomID)
	if err != nil {
		state.AvailableModIDs = normalizedIDs(append(state.AvailableModIDs, checked.ModIDs...))
		state.EmptySince = nil
		return s.fail(state, "MOD_UPDATE_CHECK_FAILED", err, timePointer(s.now().Add(time.Minute)))
	}
	state = s.recordCheck(state, checked.ModIDs)
	// Re-read telemetry immediately before restarting. A joining player cancels
	// the unattended activation even if the earlier grace observation was empty.
	presence, presenceErr = s.refreshAffectedPresence(ctx, affected)
	if presenceErr != nil || !presence.Fresh || presence.Online > 0 {
		state.EmptySince = nil
		if presenceErr != nil {
			return s.fail(state, "PLAYER_PRESENCE_RECHECK_FAILED", presenceErr, timePointer(now.Add(time.Minute)))
		}
		state.OnlinePlayers, state.StaleOnlinePlayers, state.PresenceFresh = presence.Online, presence.StaleOnline, presence.Fresh
		state.Status, state.NextActionAt = StatusWaitingForPlayers, timePointer(now.Add(time.Minute))
		if !presence.Fresh {
			state.Status = StatusBlocked
			state.ErrorCode, state.ErrorMessage = "PLAYER_PRESENCE_STALE", "玩家遥测不完整，已取消本次自动重启"
		}
		return s.saveState(state)
	}
	ctx = maintenance.WithCheck(ctx, func(ctx context.Context) error {
		latest, err := s.store.Policy(roomID)
		if err != nil {
			return err
		}
		if !latest.AutoCheck || !latest.AutoPrepare || !latest.ApplyWhenEmpty {
			return errors.New("自动模组维护已暂停")
		}
		return s.requireEmpty(ctx, affected)
	})
	if err := maintenance.Check(ctx); err != nil {
		return s.fail(state, "MAINTENANCE_RECHECK_FAILED", err, timePointer(s.now().Add(time.Minute)))
	}
	if len(checked.ModIDs) > 0 {
		state, err = s.downloadUpdates(ctx, state, checked.ModIDs)
		if err != nil {
			return state, err
		}
		state.NextActionAt = timePointer(s.now())
		return s.saveState(state)
	}
	return s.restartPrepared(ctx, roomID, jobID, state, affected...)
}

func (s *Service) restartPrepared(ctx context.Context, roomID, jobID string, state State, affected ...string) (State, error) {
	state.Status, state.NextActionAt = StatusActivating, nil
	state.ErrorCode, state.ErrorMessage = "", ""
	var err error
	if state, err = s.saveState(state); err != nil {
		return state, err
	}
	operationprogress.Report(ctx, operationprogress.Update{Percent: 80, Message: "模组下载完成，正在重启原本运行的世界"})
	if len(affected) == 0 {
		affected = []string{roomID}
	}
	for _, id := range affected {
		if err = maintenance.Check(ctx); err != nil {
			break
		}
		if err = s.restarter.RestartRunningWorlds(ctx, id, jobID); err != nil {
			break
		}
	}
	state.EmptySince = nil
	if err != nil {
		return s.fail(state, "MOD_WORLD_RESTART_FAILED", err, nil)
	}
	state.Status, state.PreparedPlanHash, state.PublicationID = StatusLoaded, "", ""
	state.AvailableModIDs = []string{}
	state.PreparedAt, state.NextActionAt = nil, nil
	state.ErrorCode, state.ErrorMessage, state.AnnouncementPlanHash = "", "", ""
	operationprogress.Report(ctx, operationprogress.Update{Percent: 100, Message: "模组已更新，原本运行的世界已重启"})
	return s.saveState(state)
}

func (s *Service) Run(ctx context.Context) {
	for {
		due, next, err := s.store.Schedule(s.now().UTC())
		if err == nil && len(due) > 0 {
			for _, item := range due {
				_, _ = s.submit(item.RoomID, item.Action)
			}
			if !waitForWake(ctx, s.wake, time.Second) {
				return
			}
			continue
		}
		var delay time.Duration
		if err != nil {
			delay = time.Minute
		} else if next != nil {
			delay = time.Until(*next)
			if delay < time.Second {
				delay = time.Second
			}
		}
		if delay == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		if !waitForWake(ctx, s.wake, delay) {
			return
		}
	}
}

func (s *Service) fail(state State, code string, cause error, nextAction *time.Time) (State, error) {
	state.Status, state.ErrorCode, state.ErrorMessage = StatusBlocked, code, cause.Error()
	state.NextActionAt = nextAction
	saved, err := s.saveState(state)
	return saved, errors.Join(cause, err)
}

func (s *Service) saveState(state State) (State, error) {
	value, err := s.store.SaveState(state)
	if err == nil {
		s.jobs.Notify()
		s.signal()
	}
	return value, err
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) clearActive(roomID, jobID string) {
	s.activeMu.Lock()
	if s.active[roomID] == jobID {
		delete(s.active, roomID)
	}
	s.activeMu.Unlock()
	s.signal()
}

func normalizePolicyInput(input *PolicyInput) error {
	if input == nil || input.RestartWithPlayers || input.EmptyGraceSeconds < 30 || input.EmptyGraceSeconds > 1800 ||
		input.CheckIntervalMinutes < 5 || input.CheckIntervalMinutes > 1440 || input.ApplyWhenEmpty && !input.AutoPrepare {
		return ErrInvalidInput
	}
	input.ExpectedRevision = strings.TrimSpace(input.ExpectedRevision)
	return nil
}

func updateJobTarget(roomID string) string {
	digest := sha256.Sum256([]byte(roomID))
	return "mod-update-" + hex.EncodeToString(digest[:])
}

func updateErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return "INVALID_MOD_UPDATE"
	case errors.Is(err, ErrRevisionConflict):
		return "MOD_UPDATE_REVISION_CONFLICT"
	case errors.Is(err, modpublication.ErrPreviewBlocked):
		return "MOD_UPDATE_PREVIEW_BLOCKED"
	case errors.Is(err, modpublication.ErrPlanChanged):
		return "MOD_UPDATE_PLAN_CHANGED"
	case errors.Is(err, modpublication.ErrTopologyChanged):
		return "MOD_UPDATE_TOPOLOGY_CHANGED"
	case errors.Is(err, modpublication.ErrActivationFailed):
		return "MOD_UPDATE_ACTIVATION_FAILED"
	default:
		return "MOD_UPDATE_FAILED"
	}
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func waitForWake(ctx context.Context, wake <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}
