package httpapi

import (
	"context"
	"crypto/sha256"
	"dont/internal/installationmaintenance"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/gameupdate"
	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
)

type GameUpdateHandler struct {
	releaseMu      sync.Mutex
	activeReleases map[string]releaseSubmission

	updates  *gameupdate.Service
	jobs     *jobs.Service
	releases GameReleaseService
	versions interface {
		Installed(context.Context, []string) ([]gameupdate.InstalledVersion, error)
	}
}

const gameReleasePreviewTimeout = 60 * time.Second

type GameReleaseService interface {
	Preview(context.Context, gameupdate.ReleasePreviewRequest) (gameupdate.ReleasePlan, error)
	Publish(context.Context, gameupdate.ReleasePublishRequest) (gameupdate.Release, error)
	Retry(context.Context, string, ...string) (gameupdate.Release, error)
	Get(string) (gameupdate.Release, error)
	List(int, int) ([]gameupdate.Release, int, error)
}

func NewGameUpdateHandler(updates *gameupdate.Service, jobService *jobs.Service) *GameUpdateHandler {
	return &GameUpdateHandler{updates: updates, jobs: jobService, activeReleases: make(map[string]releaseSubmission)}
}

func (h *GameUpdateHandler) ConfigureReleases(service GameReleaseService) error {
	if h == nil || service == nil {
		return errors.New("game release handler dependencies are required")
	}
	h.releases = service
	return nil
}

func (h *GameUpdateHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/game/version", h.version)
	if h.versions != nil {
		v2.GET("/game/installed-versions", h.installedVersions)
	}
	v2.POST("/game/actions/update", h.update)
	v2.GET("/game/update-runs/:jobId", h.run)
	if h.releases != nil {
		v2.POST("/game/releases/preview", h.releasePreview)
		v2.POST("/game/releases", h.releasePublish)
		v2.GET("/game/releases", h.releaseList)
		v2.GET("/game/releases/:releaseId", h.releaseGet)
		v2.POST("/game/releases/:releaseId/actions/retry", h.releaseRetry)
	}
}

func (h *GameUpdateHandler) releasePreview(c *gin.Context) {
	var request gameupdate.ReleasePreviewRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "游戏更新配置无效", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), gameReleasePreviewTimeout)
	defer cancel()
	plan, err := h.releases.Preview(ctx, request)
	if err != nil {
		gameReleaseFailure(c, err)
		return
	}
	Success(c, http.StatusOK, plan)
}

func (h *GameUpdateHandler) releasePublish(c *gin.Context) {
	var request gameupdate.ReleaseCreateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "游戏更新确认信息无效", nil)
		return
	}
	request.PlanHash = strings.ToLower(strings.TrimSpace(request.PlanHash))
	request.Confirmation = strings.ToLower(strings.TrimSpace(request.Confirmation))
	if request.PlanHash == "" || request.PlanHash != request.Confirmation {
		gameReleaseFailure(c, gameupdate.ErrReleaseConfirmation)
		return
	}
	h.releaseMu.Lock()
	if job, ok := h.activeReleaseJob(request.PlanHash); ok {
		h.releaseMu.Unlock()
		Success(c, http.StatusAccepted, job)
		return
	}
	h.releaseMu.Unlock()
	ctx, cancel := context.WithTimeout(c.Request.Context(), gameReleasePreviewTimeout)
	defer cancel()
	plan, err := h.releases.Preview(ctx, gameupdate.ReleasePreviewRequest{
		Installations:  request.Installations,
		DesiredVersion: request.DesiredVersion,
		TargetIDs:      request.TargetIDs,
		Policy:         request.Policy,
	})
	if err != nil {
		gameReleaseFailure(c, err)
		return
	}
	if !plan.Ready {
		gameReleaseFailure(c, &gameupdate.ReleasePreflightError{Blockers: plan.Blockers})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(request.PlanHash), plan.PlanHash) {
		gameReleaseFailure(c, gameupdate.ErrReleasePlanChanged)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(request.Confirmation), plan.PlanHash) {
		gameReleaseFailure(c, gameupdate.ErrReleaseConfirmation)
		return
	}
	h.releaseMu.Lock()
	defer h.releaseMu.Unlock()
	if job, ok := h.activeReleaseJob(plan.PlanHash); ok {
		Success(c, http.StatusAccepted, job)
		return
	}
	for _, pending := range h.activeReleases {
		for _, a := range plan.Installations {
			for _, b := range pending.Plan.Installations {
				if installationmaintenance.Overlaps(installationmaintenance.Resource{TargetID: a.TargetID, InstallationID: a.InstallationID, Directory: a.ServerPath}, installationmaintenance.Resource{TargetID: b.TargetID, InstallationID: b.InstallationID, Directory: b.ServerPath}) {
					gameReleaseFailure(c, &installationmaintenance.BusyError{Owner: pending.JobID})
					return
				}
			}
		}
	}
	targets := gameReleaseJobTargets(plan)
	job, err := h.jobs.SubmitFactory("game.release", "", "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			value, publishErr := h.releases.Publish(ctx, gameupdate.ReleasePublishRequest{ID: job.ID, SourceJobID: job.ID, Plan: plan})
			gameReleaseReport(report, plan, value, publishErr)
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建游戏更新任务", nil)
		return
	}
	h.activeReleases[plan.PlanHash] = releaseSubmission{JobID: job.ID, Plan: plan}
	Success(c, http.StatusAccepted, job)
}

func (h *GameUpdateHandler) releaseRetry(c *gin.Context) {
	id := strings.TrimSpace(c.Param("releaseId"))
	var request gameupdate.ReleaseRetryRequest
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "游戏恢复确认信息无效", nil)
		return
	}
	if request.PlanHash != request.Confirmation || request.ValidateUnknown && request.PlanHash == "" || request.FinishWithoutRestart && (request.PlanHash == "" || request.ValidateUnknown) {
		gameReleaseFailure(c, gameupdate.ErrReleaseConfirmation)
		return
	}
	h.releaseMu.Lock()
	defer h.releaseMu.Unlock()
	retryKey := "retry:" + id + ":" + request.PlanHash + ":" + strconv.FormatBool(request.FinishWithoutRestart)
	if job, ok := h.activeReleaseJob(retryKey); ok {
		Success(c, http.StatusAccepted, job)
		return
	}
	for key, pending := range h.activeReleases {
		if strings.HasPrefix(key, "retry:"+id+":") {
			gameReleaseFailure(c, &installationmaintenance.BusyError{Owner: pending.JobID})
			return
		}
	}
	current, err := h.releases.Get(id)
	if err != nil {
		gameReleaseFailure(c, err)
		return
	}
	if current.Stage != gameupdate.ReleaseStageFailed && current.Stage != gameupdate.ReleaseStageRecoveryRequired {
		gameReleaseFailure(c, gameupdate.ErrReleaseConflict)
		return
	}
	job, err := h.jobs.SubmitFactory("game.release.retry", "", "", gameReleaseJobTargets(current.Plan), func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			var value gameupdate.Release
			var retryErr error
			if request.PlanHash != "" || request.FinishWithoutRestart {
				if service, ok := h.releases.(interface {
					RetryConfirmed(context.Context, string, gameupdate.ReleaseRetryRequest, string) (gameupdate.Release, error)
				}); ok {
					value, retryErr = service.RetryConfirmed(ctx, id, request, job.ID)
				} else {
					retryErr = gameupdate.ErrReleaseInvalid
				}
			} else {
				value, retryErr = h.releases.Retry(ctx, id, job.ID)
			}
			gameReleaseReport(report, current.Plan, value, retryErr)
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建游戏版本恢复任务", nil)
		return
	}
	h.activeReleases[retryKey] = releaseSubmission{JobID: job.ID, Plan: current.Plan}
	Success(c, http.StatusAccepted, job)
}

func (h *GameUpdateHandler) releaseGet(c *gin.Context) {
	value, err := h.releases.Get(c.Param("releaseId"))
	if err != nil {
		gameReleaseFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *GameUpdateHandler) releaseList(c *gin.Context) {
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "10"))
	offset, offsetErr := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limitErr != nil || offsetErr != nil || limit < 1 || limit > 100 || offset < 0 {
		Failure(c, http.StatusUnprocessableEntity, "INVALID_PAGINATION", "分页参数无效", nil)
		return
	}
	values, total, err := h.releases.List(limit, offset)
	if err != nil {
		gameReleaseFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": values, "total": total, "limit": limit, "offset": offset})
}

func gameReleaseJobTargets(plan gameupdate.ReleasePlan) []jobs.TargetSpec {
	values := make([]jobs.TargetSpec, 0)
	for _, installation := range plan.Installations {
		values = append(values, jobs.TargetSpec{
			ID:   gameReleaseJobTarget("installation", installation.TargetID, installation.InstallationID),
			Name: installation.TargetName + " / " + installation.InstallationID,
		})
		for _, shard := range installation.Shards {
			values = append(values, jobs.TargetSpec{
				ID:   gameReleaseJobTarget("shard", shard.RoomID, shard.WorldID),
				Name: shard.RoomName + " / " + shard.WorldName,
			})
		}
	}
	return values
}

func gameReleaseJobTarget(kind, first, second string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + first + "\x00" + second))
	return "game-release:" + kind + ":" + hex.EncodeToString(digest[:12])
}

func gameReleaseReport(report func(jobs.TargetResult), plan gameupdate.ReleasePlan, value gameupdate.Release, releaseErr error) {
	for _, installation := range plan.Installations {
		result := findGameReleaseInstallation(value, installation.TargetID, installation.InstallationID)
		target := jobs.TargetResult{TargetID: gameReleaseJobTarget("installation", installation.TargetID, installation.InstallationID)}
		if result != nil && (result.Stage == gameupdate.ReleaseStageVerified || result.Stage == gameupdate.ReleaseStageSucceeded) {
			desiredVersion := installation.DesiredVersion
			if desiredVersion == "" {
				desiredVersion = plan.DesiredVersion
			}
			if result.AfterVersion != "" {
				desiredVersion = result.AfterVersion
			}
			target.Status, target.Message = jobs.StatusSucceeded, "安装已验证为 "+desiredVersion
		} else {
			code, message := releaseError(result)
			if code == "" {
				code, message = value.ErrorCode, value.ErrorMessage
			}
			target.Status, target.Error = jobs.StatusFailed, gameReleaseJobError(releaseErr, code, message)
		}
		report(target)
		for _, shard := range installation.Shards {
			shardResult := findGameReleaseShard(value, shard.RoomID, shard.WorldID)
			target := jobs.TargetResult{TargetID: gameReleaseJobTarget("shard", shard.RoomID, shard.WorldID)}
			switch {
			case releaseErr == nil && value.Stage == gameupdate.ReleaseStageAbandoned:
				target.Status, target.Message = jobs.StatusSucceeded, "已结束维护，世界保持当前状态"
			case !shard.WasRunning && shardResult != nil && shardResult.Stage == gameupdate.ReleaseStageSucceeded:
				target.Status, target.Message = jobs.StatusSucceeded, "更新前已停止，保持停止"
			case shardResult != nil && shardResult.Stage == gameupdate.ReleaseStageSucceeded && shardResult.RuntimeState == "stopped":
				target.Status, target.Message = jobs.StatusSucceeded, "按本次策略保持停止"
			case shardResult != nil && shardResult.Stage == gameupdate.ReleaseStageSucceeded:
				target.Status, target.Message = jobs.StatusSucceeded, "分片已恢复运行并完成确认"
			default:
				code, message := shardError(shardResult)
				if code == "" {
					code, message = value.ErrorCode, value.ErrorMessage
				}
				target.Status, target.Error = jobs.StatusFailed, gameReleaseJobError(releaseErr, code, message)
			}
			report(target)
		}
	}
}

func findGameReleaseInstallation(value gameupdate.Release, targetID, installationID string) *gameupdate.ReleaseInstallationResult {
	for index := range value.Installations {
		if value.Installations[index].TargetID == targetID && value.Installations[index].InstallationID == installationID {
			return &value.Installations[index]
		}
	}
	return nil
}

func findGameReleaseShard(value gameupdate.Release, roomID, worldID string) *gameupdate.ReleaseShardResult {
	for index := range value.Shards {
		if value.Shards[index].RoomID == roomID && value.Shards[index].WorldID == worldID {
			return &value.Shards[index]
		}
	}
	return nil
}

func releaseError(value *gameupdate.ReleaseInstallationResult) (string, string) {
	if value == nil {
		return "", ""
	}
	return value.ErrorCode, value.ErrorMessage
}

func shardError(value *gameupdate.ReleaseShardResult) (string, string) {
	if value == nil {
		return "", ""
	}
	return value.ErrorCode, value.ErrorMessage
}

func gameReleaseJobError(releaseErr error, code, message string) *jobs.Error {
	if code == "" {
		var blocked *gameupdate.ReleasePreflightError
		switch {
		case errors.As(releaseErr, &blocked) && len(blocked.Blockers) > 0:
			code = blocked.Blockers[0].Code
		case errors.Is(releaseErr, gameupdate.ErrReleasePlanChanged):
			code = "PLAN_CHANGED"
		case errors.Is(releaseErr, gameupdate.ErrDesiredVersionChanged):
			code = "DESIRED_VERSION_CHANGED"
		case errors.Is(releaseErr, gameupdate.ErrReleaseTopologyChanged):
			code = "TOPOLOGY_CHANGED"
		case errors.Is(releaseErr, installationmaintenance.ErrBusy):
			code = "INSTALLATION_MAINTENANCE_BUSY"
		case errors.Is(releaseErr, context.Canceled):
			code = "CANCELED"
		default:
			code = "GAME_RELEASE_INCOMPLETE"
		}
	}
	if message == "" && releaseErr != nil {
		message = releaseErr.Error()
	}
	if message == "" {
		message = "游戏更新未完成，请检查更新记录"
	}
	return &jobs.Error{Code: code, Message: message}
}

func gameReleaseFailure(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "GAME_RELEASE_FAILED", "游戏更新操作失败"
	var blocked *gameupdate.ReleasePreflightError
	if errors.As(err, &blocked) {
		Failure(c, http.StatusConflict, "PREVIEW_BLOCKED", blocked.Error(), gin.H{"blockers": blocked.Blockers})
		return
	}
	var busy *installationmaintenance.BusyError
	if errors.As(err, &busy) {
		Failure(c, http.StatusConflict, "INSTALLATION_MAINTENANCE_BUSY", "该游戏安装正在维护，请查看原任务", gin.H{"releaseId": busy.Owner})
		return
	}
	switch {
	case errors.Is(err, gameupdate.ErrReleaseInstallationUnused):
		status, code, message = http.StatusUnprocessableEntity, "INSTALLATION_UNUSED", "该安装没有受管世界，请使用游戏安装管理执行更新或校验"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusGatewayTimeout, "GAME_RELEASE_PREVIEW_TIMEOUT", "版本检查超时，Steam 或运行节点响应过慢，请稍后重试"
	case errors.Is(err, gameupdate.ErrReleaseNotFound):
		status, code, message = http.StatusNotFound, "GAME_RELEASE_NOT_FOUND", "游戏更新记录不存在"
	case errors.Is(err, gameupdate.ErrLatestBuildUnavailable):
		status, code, message = http.StatusServiceUnavailable, "STEAM_BUILD_UNAVAILABLE", "暂时无法从 Steam 获取最新 build，请稍后重试"
	case errors.Is(err, gameupdate.ErrReleaseInvalid):
		status, code, message = http.StatusUnprocessableEntity, "INVALID_GAME_RELEASE", "游戏更新请求无效"
	case errors.Is(err, gameupdate.ErrReleaseConfirmation):
		status, code, message = http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "必须使用当前检查计划确认更新"
	case errors.Is(err, gameupdate.ErrReleasePreviewBlocked):
		status, code, message = http.StatusConflict, "PREVIEW_BLOCKED", "游戏更新检查存在阻断项"
	case errors.Is(err, gameupdate.ErrDesiredVersionChanged):
		status, code, message = http.StatusConflict, "DESIRED_VERSION_CHANGED", "Steam 目标版本已变化，请重新检查"
	case errors.Is(err, gameupdate.ErrReleasePlanChanged):
		status, code, message = http.StatusConflict, "PLAN_CHANGED", "游戏更新计划已变化，请重新检查"
	case errors.Is(err, gameupdate.ErrReleaseTopologyChanged):
		status, code, message = http.StatusConflict, "TOPOLOGY_CHANGED", "运行拓扑已变化，请重新检查"
	case errors.Is(err, gameupdate.ErrReleaseConflict):
		status, code, message = http.StatusConflict, "GAME_RELEASE_CONFLICT", "游戏更新状态冲突"
	case errors.Is(err, gameupdate.ErrReleaseRecoveryNeeded):
		status, code, message = http.StatusConflict, "GAME_RELEASE_RECOVERY_REQUIRED", "游戏更新需要人工检查或恢复"
	}
	Failure(c, status, code, message, nil)
}

func (h *GameUpdateHandler) version(c *gin.Context) {
	if c.Query("steam") == "false" {
		Success(c, http.StatusOK, h.updates.VersionSummary(c.Request.Context(), c.Query("refresh") == "true"))
		return
	}
	Success(c, http.StatusOK, h.updates.Version(c.Request.Context()))
}

func (h *GameUpdateHandler) ConfigureInstalledVersions(service interface {
	Installed(context.Context, []string) ([]gameupdate.InstalledVersion, error)
}) {
	h.versions = service
}

func (h *GameUpdateHandler) installedVersions(c *gin.Context) {
	var ids []string
	if raw := c.Query("targetIds"); raw != "" {
		ids = strings.Split(raw, ",")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	values, err := h.versions.Installed(ctx, ids)
	if err != nil {
		gameReleaseFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"installations": values})
}

func (h *GameUpdateHandler) update(c *gin.Context) {
	var request gameupdate.UpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的更新配置", nil)
		return
	}
	targets, factory, release, err := h.updates.Prepare(c.Request.Context(), request)
	if err != nil {
		gameUpdateFailure(c, err)
		return
	}
	job, err := h.jobs.SubmitFactory("game.update", "", "", targets, factory)
	if err != nil {
		release()
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建游戏更新任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *GameUpdateHandler) run(c *gin.Context) {
	value, err := h.updates.Run(c.Param("jobId"))
	if errors.Is(err, gameupdate.ErrRunNotFound) {
		if job, jobErr := h.jobs.Get(c.Param("jobId")); jobErr == nil {
			if pending, ok := gameUpdateRunFromJob(job); ok {
				Success(c, http.StatusOK, pending)
				return
			}
		}
	}
	if err != nil {
		gameUpdateFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func gameUpdateRunFromJob(job jobs.Job) (gameupdate.Run, bool) {
	if job.Kind != "game.update" {
		return gameupdate.Run{}, false
	}
	startedAt := job.CreatedAt
	if job.StartedAt != nil {
		startedAt = *job.StartedAt
	}
	errorMessage := ""
	if job.Error != nil {
		errorMessage = job.Error.Message
	}
	return gameupdate.Run{
		JobID: job.ID, Status: string(job.Status), ErrorMessage: errorMessage,
		StartedAt: startedAt, FinishedAt: job.FinishedAt,
	}, true
}

func gameUpdateFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, gameupdate.ErrRunNotFound):
		Failure(c, http.StatusNotFound, "UPDATE_RUN_NOT_FOUND", "更新记录不存在或尚未开始", nil)
	case errors.Is(err, gameupdate.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入“更新游戏”确认操作", nil)
	case errors.Is(err, gameupdate.ErrUpdateInProgress):
		Failure(c, http.StatusConflict, "UPDATE_IN_PROGRESS", "已有游戏更新正在执行", nil)
	case errors.Is(err, gameupdate.ErrSteamCMDUnavailable):
		Failure(c, http.StatusConflict, "STEAMCMD_UNAVAILABLE", "SteamCMD 不可用，请先完成部署检查", nil)
	case errors.Is(err, gameupdate.ErrSteamClientManaged):
		Failure(c, http.StatusConflict, "STEAM_CLIENT_MANAGED", "当前游戏由 Steam 客户端管理，请在 Steam 中完成更新", nil)
	case errors.Is(err, gameupdate.ErrUpdateDisabled):
		Failure(c, http.StatusConflict, "LOCAL_GAME_UPDATE_DISABLED", "当前控制面不管理本地 DST 安装，请在对应 Runtime 节点执行更新", nil)
	default:
		Failure(c, http.StatusInternalServerError, "GAME_UPDATE_FAILED", "游戏更新操作失败", nil)
	}
}

type releaseSubmission struct {
	JobID string
	Plan  gameupdate.ReleasePlan
}

// Called under releaseMu. Completed entries are pruned on demand; no polling
// worker or unbounded history map is required for request coalescing.
func (h *GameUpdateHandler) activeReleaseJob(hash string) (jobs.Job, bool) {
	var match jobs.Job
	found := false
	for key, pending := range h.activeReleases {
		job, err := h.jobs.Get(pending.JobID)
		if err != nil || job.Status != jobs.StatusRunning && job.Status != jobs.StatusQueued {
			delete(h.activeReleases, key)
			continue
		}
		if key == hash {
			match, found = job, true
		}
	}
	return match, found
}
