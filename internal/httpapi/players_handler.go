package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"dont/internal/agents"
	"dont/internal/configuration"
	"dont/internal/dstruntime"
	"dont/internal/jobs"
	"dont/internal/players"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"

	"github.com/gin-gonic/gin"
)

type PlayerService interface {
	List(string, players.ListFilter) (players.List, error)
	Player(string, string) (players.Player, error)
	WorldTargets(string) ([]players.WorldTarget, error)
	RefreshWorld(context.Context, string, string) (players.RefreshResult, error)
	Act(context.Context, string, string, string, players.Action, players.ActionRequest) (players.ActionResult, error)
}

type playerBatchRefresher interface {
	RefreshWorlds(context.Context, string, []string) ([]players.RefreshOutcome, error)
}

type PlayerHandler struct {
	players PlayerService
	jobs    *jobs.Service
}

type playerRefreshRequest struct {
	WorldIDs []string `json:"worldIds"`
}

func NewPlayerHandler(service PlayerService, jobService *jobs.Service) *PlayerHandler {
	return &PlayerHandler{players: service, jobs: jobService}
}

func (h *PlayerHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/rooms/:roomId/players")
	group.GET("", h.list)
	group.GET("/:playerId", h.player)
	group.POST("/actions/refresh", h.refresh)
	group.POST("/:playerId/actions/kick", h.action(players.ActionKick))
	group.POST("/:playerId/actions/ban", h.action(players.ActionBan))
	group.POST("/:playerId/actions/unban", h.action(players.ActionUnban))
	group.POST("/:playerId/actions/announce", h.action(players.ActionAnnounce))
	group.POST("/:playerId/actions/kill", h.action(players.ActionKill))
	group.POST("/:playerId/actions/god-mode", h.action(players.ActionGodMode))
	group.POST("/:playerId/actions/creative-mode", h.action(players.ActionCreativeMode))
	group.POST("/:playerId/actions/resurrect", h.action(players.ActionResurrect))
	group.POST("/:playerId/actions/change-character", h.action(players.ActionChangeCharacter))
}

func (h *PlayerHandler) list(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	includeAccessLists := true
	if raw, exists := c.GetQuery("includeAccessLists"); exists {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			playerFailure(c, players.ErrInvalidFilter)
			return
		}
		includeAccessLists = parsed
	}
	value, err := h.players.List(c.Param("roomId"), players.ListFilter{
		Query: c.Query("query"), Status: c.Query("status"), WorldID: c.Query("worldId"),
		Prefab: c.Query("prefab"), Limit: limit, Offset: offset, SkipAccessLists: !includeAccessLists,
	})
	if err != nil {
		playerFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *PlayerHandler) player(c *gin.Context) {
	value, err := h.players.Player(c.Param("roomId"), c.Param("playerId"))
	if err != nil {
		playerFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *PlayerHandler) refresh(c *gin.Context) {
	var request playerRefreshRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的玩家刷新操作", nil)
		return
	}
	targets, err := h.players.WorldTargets(c.Param("roomId"))
	if err != nil {
		playerFailure(c, err)
		return
	}
	targets, err = selectPlayerRefreshTargets(targets, request.WorldIDs)
	if err != nil {
		Failure(c, http.StatusUnprocessableEntity, "INVALID_PLAYER_REFRESH", "选择的刷新世界无效", nil)
		return
	}
	jobTargets := make([]jobs.TargetSpec, 0, len(targets))
	for _, target := range targets {
		jobTargets = append(jobTargets, jobs.TargetSpec{ID: target.ID, Name: target.Name})
	}
	roomID := c.Param("roomId")
	job, err := h.jobs.Submit("player.refresh", roomID, "", jobTargets, func(ctx context.Context, report func(jobs.TargetResult)) error {
		if batch, supported := h.players.(playerBatchRefresher); supported {
			worldIDs := make([]string, 0, len(targets))
			for _, target := range targets {
				worldIDs = append(worldIDs, target.ID)
			}
			outcomes, refreshErr := batch.RefreshWorlds(ctx, roomID, worldIDs)
			if refreshErr != nil {
				return refreshErr
			}
			var result error
			for _, outcome := range outcomes {
				if outcome.Err != nil {
					report(jobs.TargetResult{TargetID: outcome.WorldID, Status: jobs.StatusFailed, Error: playerJobError(outcome.Err)})
					result = errors.Join(result, outcome.Err)
					continue
				}
				report(jobs.TargetResult{TargetID: outcome.WorldID, Status: jobs.StatusSucceeded, Message: outcome.Result.Message})
			}
			return result
		}
		var result error
		for _, target := range targets {
			refreshed, refreshErr := h.players.RefreshWorld(ctx, roomID, target.ID)
			if refreshErr != nil {
				report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusFailed, Error: playerJobError(refreshErr)})
				result = errors.Join(result, refreshErr)
				continue
			}
			report(jobs.TargetResult{TargetID: target.ID, Status: jobs.StatusSucceeded, Message: refreshed.Message})
		}
		return result
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建玩家刷新任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func selectPlayerRefreshTargets(targets []players.WorldTarget, worldIDs []string) ([]players.WorldTarget, error) {
	if len(worldIDs) == 0 {
		return targets, nil
	}
	if len(worldIDs) > 64 {
		return nil, players.ErrInvalidFilter
	}
	available := make(map[string]players.WorldTarget, len(targets))
	for _, target := range targets {
		available[target.ID] = target
	}
	selected := make([]players.WorldTarget, 0, len(worldIDs))
	seen := make(map[string]bool, len(worldIDs))
	for _, worldID := range worldIDs {
		target, exists := available[worldID]
		if !exists || seen[worldID] {
			return nil, players.ErrInvalidFilter
		}
		seen[worldID] = true
		selected = append(selected, target)
	}
	return selected, nil
}

func (h *PlayerHandler) action(action players.Action) gin.HandlerFunc {
	return func(c *gin.Context) {
		playerID := c.Param("playerId")
		if !players.ValidID(playerID) {
			playerFailure(c, players.ErrInvalidPlayer)
			return
		}
		var request players.ActionRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的玩家操作", nil)
			return
		}
		player, err := h.players.Player(c.Param("roomId"), playerID)
		if err != nil {
			playerFailure(c, err)
			return
		}
		roomID := c.Param("roomId")
		targets := []jobs.TargetSpec{{ID: player.ID, Name: player.Name}}
		job, err := h.jobs.SubmitFactory("player."+string(action), roomID, request.WorldID, targets, func(job jobs.Job) jobs.Runner {
			return func(ctx context.Context, report func(jobs.TargetResult)) error {
				result, actionErr := h.players.Act(ctx, job.ID, roomID, player.ID, action, request)
				if actionErr != nil {
					report(jobs.TargetResult{TargetID: player.ID, Status: jobs.StatusFailed, Error: playerJobError(actionErr)})
					return actionErr
				}
				message := result.Message
				if result.Warning != "" {
					message += "；" + result.Warning
				}
				report(jobs.TargetResult{TargetID: player.ID, Status: jobs.StatusSucceeded, Message: message})
				return nil
			}
		})
		if err != nil {
			Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建玩家操作任务", nil)
			return
		}
		Success(c, http.StatusAccepted, job)
	}
}

func playerFailure(c *gin.Context, err error) {
	var fieldErr *players.FieldError
	switch {
	case errors.As(err, &fieldErr):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_PLAYER_ACTION", "玩家操作参数无效", fieldErr.Fields)
	case errors.Is(err, players.ErrInvalidPlayer), errors.Is(err, players.ErrInvalidFilter):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_PLAYER_INPUT", "玩家查询或标识无效", nil)
	case errors.Is(err, players.ErrPlayerNotFound):
		Failure(c, http.StatusNotFound, "PLAYER_NOT_FOUND", "玩家不存在", nil)
	case errors.Is(err, players.ErrConfirmationRequired), errors.Is(err, configuration.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "确认内容不匹配", nil)
	case errors.Is(err, players.ErrRoomNotManaged), errors.Is(err, configuration.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, players.ErrWorldNotRunning):
		Failure(c, http.StatusConflict, "WORLD_NOT_RUNNING", "目标分片未运行", nil)
	case errors.Is(err, agents.ErrAgentOffline), errors.Is(err, agents.ErrUnavailable):
		Failure(c, http.StatusServiceUnavailable, "AGENT_UNAVAILABLE", "目标 Agent 当前离线或连接不可用", nil)
	case agentUpgradeRequired(err):
		Failure(c, http.StatusConflict, "AGENT_UPGRADE_REQUIRED", "目标 Agent 版本过旧，缺少玩家管理所需能力，请升级 Agent 后重试", nil)
	case errors.Is(err, topology.ErrExecutionBlocked):
		Failure(c, http.StatusConflict, "PLAYER_TARGET_UNAVAILABLE", err.Error(), nil)
	case errors.Is(err, dstruntime.ErrRuntimeNotInstalled):
		Failure(c, http.StatusConflict, "RUNTIME_NOT_INSTALLED", "目标分片尚未安装玩家采集 Runtime", nil)
	case errors.Is(err, dstruntime.ErrRuntimeUnavailable):
		Failure(c, http.StatusServiceUnavailable, "RUNTIME_UNAVAILABLE", "目标分片 Runtime 当前不可用", nil)
	case errors.Is(err, dstruntime.ErrSnapshotUnavailable), errors.Is(err, dstruntime.ErrSnapshotStale):
		Failure(c, http.StatusServiceUnavailable, "PLAYER_SNAPSHOT_UNAVAILABLE", "目标分片尚未产生可用的玩家快照，请确认游戏已启动后重试", nil)
	case errors.Is(err, runtimedriver.ErrOperationNotDispatched):
		Failure(c, http.StatusServiceUnavailable, "PLAYER_COMMAND_NOT_DISPATCHED", "玩家命令未能发送到目标运行节点", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间或世界标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	default:
		log.Printf("[PlayersHTTP] request_id=%s method=%s path=%s error=%v", RequestID(c), c.Request.Method, c.Request.URL.Path, err)
		Failure(c, http.StatusInternalServerError, "PLAYER_OPERATION_FAILED", "玩家操作失败", nil)
	}
}

func playerJobError(err error) *jobs.Error {
	code := "PLAYER_OPERATION_FAILED"
	switch {
	case errors.Is(err, players.ErrWorldNotRunning):
		code = "WORLD_NOT_RUNNING"
	case errors.Is(err, players.ErrProbeTimedOut):
		code = "PLAYER_PROBE_TIMEOUT"
	case errors.Is(err, players.ErrConfirmationRequired):
		code = "CONFIRMATION_REQUIRED"
	case errors.Is(err, players.ErrInvalidAction):
		code = "INVALID_PLAYER_ACTION"
	case errors.Is(err, agents.ErrAgentOffline), errors.Is(err, agents.ErrUnavailable):
		code = "AGENT_UNAVAILABLE"
	case agentUpgradeRequired(err):
		code = "AGENT_UPGRADE_REQUIRED"
	case errors.Is(err, topology.ErrExecutionBlocked):
		code = "PLAYER_TARGET_UNAVAILABLE"
	case errors.Is(err, dstruntime.ErrRuntimeNotInstalled):
		code = "RUNTIME_NOT_INSTALLED"
	case errors.Is(err, dstruntime.ErrRuntimeUnavailable):
		code = "RUNTIME_UNAVAILABLE"
	case errors.Is(err, dstruntime.ErrSnapshotUnavailable), errors.Is(err, dstruntime.ErrSnapshotStale):
		code = "PLAYER_SNAPSHOT_UNAVAILABLE"
	case errors.Is(err, runtimedriver.ErrOperationNotDispatched):
		code = "PLAYER_COMMAND_NOT_DISPATCHED"
	}
	return &jobs.Error{Code: code, Message: err.Error()}
}
