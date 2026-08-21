package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dont/internal/gameupdate"
	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
)

type GameUpdateHandler struct {
	updates  *gameupdate.Service
	jobs     *jobs.Service
	releases GameReleaseService
}

type GameReleaseService interface {
	Preview(context.Context, gameupdate.ReleasePreviewRequest) (gameupdate.ReleasePlan, error)
	Publish(context.Context, gameupdate.ReleasePublishRequest) (gameupdate.Release, error)
	Retry(context.Context, string, ...string) (gameupdate.Release, error)
	Get(string) (gameupdate.Release, error)
	List(int, int) ([]gameupdate.Release, int, error)
}

func NewGameUpdateHandler(updates *gameupdate.Service, jobService *jobs.Service) *GameUpdateHandler {
	return &GameUpdateHandler{updates: updates, jobs: jobService}
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
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的版本发布配置", nil)
		return
	}
	plan, err := h.releases.Preview(c.Request.Context(), request)
	if err != nil {
		gameReleaseFailure(c, err)
		return
	}
	Success(c, http.StatusOK, plan)
}

func (h *GameUpdateHandler) releasePublish(c *gin.Context) {
	var request gameupdate.ReleaseCreateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的版本发布确认", nil)
		return
	}
	plan, err := h.releases.Preview(c.Request.Context(), gameupdate.ReleasePreviewRequest{
		DesiredVersion: request.DesiredVersion,
		Policy:         request.Policy,
	})
	if err != nil {
		gameReleaseFailure(c, err)
		return
	}
	if !plan.Ready {
		gameReleaseFailure(c, gameupdate.ErrReleasePreviewBlocked)
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
	targets := gameReleaseJobTargets(plan)
	job, err := h.jobs.SubmitFactory("game.release", "", "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			value, publishErr := h.releases.Publish(ctx, gameupdate.ReleasePublishRequest{ID: job.ID, SourceJobID: job.ID, Plan: plan})
			gameReleaseReport(report, plan, value, publishErr)
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建游戏版本发布任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *GameUpdateHandler) releaseRetry(c *gin.Context) {
	id := strings.TrimSpace(c.Param("releaseId"))
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
			value, retryErr := h.releases.Retry(ctx, id, job.ID)
			gameReleaseReport(report, current.Plan, value, retryErr)
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建游戏版本恢复任务", nil)
		return
	}
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
			target.Status, target.Message = jobs.StatusSucceeded, "安装已更新并验证为 "+plan.DesiredVersion
		} else {
			code, message := releaseError(result)
			target.Status, target.Error = jobs.StatusFailed, gameReleaseJobError(releaseErr, code, message)
		}
		report(target)
		for _, shard := range installation.Shards {
			shardResult := findGameReleaseShard(value, shard.RoomID, shard.WorldID)
			target := jobs.TargetResult{TargetID: gameReleaseJobTarget("shard", shard.RoomID, shard.WorldID)}
			switch {
			case !shard.WasRunning && shardResult != nil && shardResult.Stage == gameupdate.ReleaseStageSucceeded:
				target.Status, target.Message = jobs.StatusSucceeded, "发布前已停止，保持停止"
			case shardResult != nil && shardResult.Stage == gameupdate.ReleaseStageSucceeded:
				target.Status, target.Message = jobs.StatusSucceeded, "分片已恢复运行并完成加载确认"
			default:
				code, message := shardError(shardResult)
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
		code = "GAME_RELEASE_INCOMPLETE"
	}
	if message == "" && releaseErr != nil {
		message = releaseErr.Error()
	}
	if message == "" {
		message = "游戏版本发布未完成，请检查发布记录"
	}
	return &jobs.Error{Code: code, Message: message}
}

func gameReleaseFailure(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "GAME_RELEASE_FAILED", "游戏版本发布操作失败"
	switch {
	case errors.Is(err, gameupdate.ErrReleaseNotFound):
		status, code, message = http.StatusNotFound, "GAME_RELEASE_NOT_FOUND", "游戏版本发布记录不存在"
	case errors.Is(err, gameupdate.ErrReleaseInvalid):
		status, code, message = http.StatusUnprocessableEntity, "INVALID_GAME_RELEASE", "游戏版本发布请求无效"
	case errors.Is(err, gameupdate.ErrReleaseConfirmation):
		status, code, message = http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "必须使用当前 planHash 确认发布"
	case errors.Is(err, gameupdate.ErrReleasePreviewBlocked):
		status, code, message = http.StatusConflict, "PREVIEW_BLOCKED", "游戏版本发布预检存在阻断项"
	case errors.Is(err, gameupdate.ErrDesiredVersionChanged):
		status, code, message = http.StatusConflict, "DESIRED_VERSION_CHANGED", "Steam 目标版本已变化，请重新预览"
	case errors.Is(err, gameupdate.ErrReleasePlanChanged):
		status, code, message = http.StatusConflict, "PLAN_CHANGED", "游戏版本发布计划已变化，请重新预览"
	case errors.Is(err, gameupdate.ErrReleaseTopologyChanged):
		status, code, message = http.StatusConflict, "TOPOLOGY_CHANGED", "运行拓扑已变化，请重新预览"
	case errors.Is(err, gameupdate.ErrReleaseConflict):
		status, code, message = http.StatusConflict, "GAME_RELEASE_CONFLICT", "游戏版本发布状态冲突"
	case errors.Is(err, gameupdate.ErrReleaseRecoveryNeeded):
		status, code, message = http.StatusConflict, "GAME_RELEASE_RECOVERY_REQUIRED", "游戏版本发布需要人工检查或恢复"
	}
	Failure(c, status, code, message, nil)
}

func (h *GameUpdateHandler) version(c *gin.Context) {
	Success(c, http.StatusOK, h.updates.Version(c.Request.Context()))
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
	if err != nil {
		gameUpdateFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
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
