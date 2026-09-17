package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"dont/internal/jobs"
	"dont/internal/operationprogress"
	"dont/internal/rooms"
	"dont/internal/runtimeaudit"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/shared"

	"github.com/gin-gonic/gin"
)

type RoomHandler struct {
	rooms      *rooms.Service
	operations *shards.Operations
	jobs       *jobs.Service
	audit      *runtimeaudit.Service
	recovery   roomRecoveryMover
}

type roomRecoveryMover interface {
	MoveRoomToRecovery(context.Context, string, []string) (runtimedriver.RoomRecoveryLocation, error)
}

func NewRoomHandler(roomService *rooms.Service, operations *shards.Operations, jobService *jobs.Service, audits ...*runtimeaudit.Service) *RoomHandler {
	handler := &RoomHandler{rooms: roomService, operations: operations, jobs: jobService}
	if len(audits) > 0 {
		handler.audit = audits[0]
	}
	return handler
}

func (h *RoomHandler) ConfigureRoomRecovery(mover roomRecoveryMover) error {
	if mover == nil {
		return errors.New("room recovery mover is required")
	}
	h.recovery = mover
	return nil
}

func (h *RoomHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/rooms")
	group.GET("", h.list)
	group.POST("", h.create)
	group.POST("/actions/:action", h.batchAction)
	group.GET("/recovery", h.roomRecoveries)
	group.POST("/recovery/:recoveryName/actions/restore", h.restoreRoom)
	group.DELETE("/recovery/:recoveryName", h.purgeRoomRecovery)
	group.GET("/:roomId", h.get)
	group.GET("/:roomId/runtime-modes", h.runtimeModes)
	group.DELETE("/:roomId", h.deleteRoom)
	group.GET("/:roomId/worlds", h.worldsList)
	group.POST("/:roomId/worlds", h.createWorld)
	group.GET("/:roomId/worlds/recovery", h.worldRecoveries)
	group.POST("/:roomId/worlds/recovery/:recoveryName/actions/restore", h.restoreWorld)
	group.DELETE("/:roomId/worlds/recovery/:recoveryName", h.purgeWorldRecovery)
	group.DELETE("/:roomId/worlds/:worldId", h.deleteWorld)
	group.POST("/:roomId/actions/:action", h.action)
}

func (h *RoomHandler) roomRecoveries(c *gin.Context) {
	items, err := h.rooms.ListRoomRecoveries()
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *RoomHandler) restoreRoom(c *gin.Context) {
	room, err := h.rooms.RestoreRoom(c.Param("recoveryName"))
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, room)
}

func (h *RoomHandler) purgeRoomRecovery(c *gin.Context) {
	var request rooms.PurgeRecoveryRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的永久清理确认", nil)
		return
	}
	if err := h.rooms.PurgeRoomRecovery(c.Param("recoveryName"), request); err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"recoveryName": c.Param("recoveryName")})
}

func (h *RoomHandler) worldRecoveries(c *gin.Context) {
	items, err := h.rooms.ListWorldRecoveries(c.Param("roomId"))
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *RoomHandler) restoreWorld(c *gin.Context) {
	roomID := c.Param("roomId")
	if err := h.requireRoomStopped(c, roomID, ""); err != nil {
		roomFailure(c, err)
		return
	}
	world, err := h.rooms.RestoreWorld(roomID, c.Param("recoveryName"))
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, world)
}

func (h *RoomHandler) purgeWorldRecovery(c *gin.Context) {
	var request rooms.PurgeRecoveryRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的永久清理确认", nil)
		return
	}
	if err := h.rooms.PurgeWorldRecovery(c.Param("roomId"), c.Param("recoveryName"), request); err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"recoveryName": c.Param("recoveryName")})
}

func (h *RoomHandler) deleteRoom(c *gin.Context) {
	var request rooms.DeleteRoomRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的删除确认", nil)
		return
	}
	if err := h.requireRoomStopped(c, c.Param("roomId"), ""); err != nil {
		roomFailure(c, err)
		return
	}
	roomID := c.Param("roomId")
	room, err := h.rooms.Room(roomID)
	if err != nil {
		roomFailure(c, err)
		return
	}
	if request.Confirmation != room.Name {
		roomFailure(c, rooms.ErrConfirmation)
		return
	}
	local, err := h.rooms.HasLocalRoom(roomID)
	if err != nil {
		roomFailure(c, err)
		return
	}
	if local {
		if len(room.TargetIDs) != 1 || room.TargetIDs[0] != runtimedriver.LocalTargetID {
			roomFailure(c, runtimedriver.ErrRoomRecoveryMultiTarget)
			return
		}
		result, deleteErr := h.rooms.DeleteRoom(roomID, request)
		if deleteErr != nil {
			roomFailure(c, deleteErr)
			return
		}
		Success(c, http.StatusOK, result)
		return
	}
	if h.recovery == nil {
		roomFailure(c, runtimedriver.ErrCapabilityMissing)
		return
	}
	worlds, err := h.rooms.Worlds(roomID)
	if err != nil {
		roomFailure(c, err)
		return
	}
	worldIDs := make([]string, 0, len(worlds))
	for _, world := range worlds {
		worldIDs = append(worldIDs, world.ID)
	}
	location, err := h.recovery.MoveRoomToRecovery(c.Request.Context(), roomID, worldIDs)
	if err != nil {
		roomFailure(c, err)
		return
	}
	forgotten, err := h.rooms.FinalizeRemoteRoomRecovery(roomID)
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, rooms.DeleteRoomResult{
		Room: forgotten, RecoveryName: location.TargetID + ":" + location.RecoveryRef,
	})
}

func (h *RoomHandler) createWorld(c *gin.Context) {
	var request rooms.CreateWorldRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的世界配置", nil)
		return
	}
	if err := h.requireRoomStopped(c, c.Param("roomId"), ""); err != nil {
		roomFailure(c, err)
		return
	}
	result, err := h.rooms.CreateWorld(c.Param("roomId"), request)
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, result)
}

func (h *RoomHandler) deleteWorld(c *gin.Context) {
	var request rooms.DeleteWorldRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的删除确认", nil)
		return
	}
	if err := h.requireRoomStopped(c, c.Param("roomId"), c.Param("worldId")); err != nil {
		roomFailure(c, err)
		return
	}
	result, err := h.rooms.DeleteWorld(c.Param("roomId"), c.Param("worldId"), request)
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, result)
}

func (h *RoomHandler) requireRoomStopped(c *gin.Context, roomID, selectedWorldID string) error {
	room, err := h.rooms.Room(roomID)
	if err != nil {
		return err
	}
	worlds, err := h.rooms.Worlds(room.ID)
	if err != nil {
		return err
	}
	for _, world := range worlds {
		if selectedWorldID != "" && world.ID != selectedWorldID {
			continue
		}
		status, statusErr := h.operations.StatusFor(c.Request.Context(), room.ID, world.ID)
		if statusErr != nil {
			return statusErr
		}
		if status.SessionExists || status.State == shards.RuntimeStarting || status.State == shards.RuntimeRunning {
			return rooms.ErrWorldRunning
		}
	}
	return nil
}

func (h *RoomHandler) list(c *gin.Context) {
	result, err := h.rooms.List()
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": result, "total": len(result)})
}

func (h *RoomHandler) create(c *gin.Context) {
	var request rooms.CreateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的房间配置", nil)
		return
	}
	result, err := h.rooms.Create(request)
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, result)
}

func (h *RoomHandler) get(c *gin.Context) {
	result, err := h.rooms.Room(c.Param("roomId"))
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, result)
}

type worldState struct {
	rooms.World
	Status           string              `json:"status"`
	StatusCode       string              `json:"statusCode,omitempty"`
	ControlAvailable bool                `json:"controlAvailable"`
	StatusMessage    string              `json:"statusMessage,omitempty"`
	Paused           *bool               `json:"paused,omitempty"`
	LatestExit       *runtimeaudit.Event `json:"latestExit,omitempty"`
}

func (h *RoomHandler) worldsList(c *gin.Context) {
	room, err := h.rooms.Room(c.Param("roomId"))
	if err != nil {
		roomFailure(c, err)
		return
	}
	worlds, err := h.rooms.Worlds(room.ID)
	if err != nil {
		roomFailure(c, err)
		return
	}
	result := make([]worldState, 0, len(worlds))
	for _, world := range worlds {
		state := worldState{World: world, Status: "unknown", ControlAvailable: true}
		status, statusErr := h.operations.StatusFor(c.Request.Context(), room.ID, world.ID)
		if statusErr != nil {
			state.ControlAvailable = false
			state.StatusMessage = statusErr.Error()
			result = append(result, state)
			continue
		}
		state.Status = string(status.State)
		state.StatusCode = status.Code
		state.StatusMessage = status.Message
		state.Paused = status.Paused
		if h.audit != nil {
			state.LatestExit, _ = h.audit.LatestExit(room.ID, world.ID)
		}
		result = append(result, state)
	}
	Success(c, http.StatusOK, gin.H{"items": result, "total": len(result)})
}

type roomActionRequest struct {
	Immediate         bool                          `json:"immediate"`
	WorldIDs          []string                      `json:"worldIds"`
	AllowCapacityRisk bool                          `json:"allowCapacityRisk"`
	RuntimeMode       shared.RuntimePerformanceMode `json:"runtimeMode"`
	RuntimeVersion    string                        `json:"runtimeVersion"`
}

type batchRoomActionSelection struct {
	RoomID            string                        `json:"roomId"`
	WorldIDs          []string                      `json:"worldIds"`
	AllowCapacityRisk bool                          `json:"allowCapacityRisk"`
	RuntimeMode       shared.RuntimePerformanceMode `json:"runtimeMode"`
	RuntimeVersion    string                        `json:"runtimeVersion"`
}

func (h *RoomHandler) runtimeModes(c *gin.Context) {
	worldIDs := make([]string, 0)
	for _, value := range c.QueryArray("worldIds") {
		for _, worldID := range strings.Split(value, ",") {
			if worldID = strings.TrimSpace(worldID); worldID != "" {
				worldIDs = append(worldIDs, worldID)
			}
		}
	}
	availability, err := h.operations.RuntimeModes(c.Request.Context(), c.Param("roomId"), worldIDs)
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, availability)
}

type batchRoomActionRequest struct {
	Rooms             []batchRoomActionSelection `json:"rooms"`
	AllowCapacityRisk bool                       `json:"allowCapacityRisk"`
}

func (h *RoomHandler) action(c *gin.Context) {
	action := shards.Action(strings.ToLower(strings.TrimSpace(c.Param("action"))))
	var request roomActionRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的操作配置", nil)
			return
		}
	}
	roomID := c.Param("roomId")
	if action == shards.ActionStart || action == shards.ActionRestart {
		if _, err := h.operations.RequireRuntimeSelection(c.Request.Context(), roomID, request.WorldIDs, request.RuntimeMode, request.RuntimeVersion); err != nil {
			roomFailure(c, err)
			return
		}
	}
	if err := h.operations.RequireCapacityConfirmation(c.Request.Context(), action, roomID, request.WorldIDs, request.AllowCapacityRisk); err != nil {
		roomFailure(c, err)
		return
	}
	targets, runner, err := h.operations.PlanWithOptions(action, roomID, request.WorldIDs, shards.PlanOptions{
		RuntimeMode: request.RuntimeMode, Immediate: request.Immediate,
	})
	if err != nil {
		roomFailure(c, err)
		return
	}
	worldID := ""
	if len(request.WorldIDs) == 1 {
		worldID = request.WorldIDs[0]
	}
	requestID := RequestID(c)
	job, err := h.jobs.SubmitFactory("room."+string(action), roomID, worldID, targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			if h.audit != nil {
				if auditErr := h.audit.RecordAction(runtimeaudit.ActionRequest{
					RoomID: roomID, WorldIDs: request.WorldIDs, Action: string(action), Source: runtimeaudit.SourceAPI,
					JobID: job.ID, RequestID: requestID,
				}); auditErr != nil {
					log.Printf("[RuntimeAudit] record API action room=%s action=%s: %v", roomID, action, auditErr)
				}
			}
			ctx = shards.WithOperationAudit(ctx, shards.OperationAuditMetadata{
				JobID: job.ID, RequestID: requestID, Source: string(runtimeaudit.SourceAPI),
			})
			return h.runActionJob(ctx, job.ID, action, targets, runner, report)
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建运行任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *RoomHandler) runActionJob(ctx context.Context, jobID string, action shards.Action, targets []jobs.TargetSpec, runner jobs.Runner, report func(jobs.TargetResult)) error {
	if action != shards.ActionStart && action != shards.ActionRestart {
		return runner(ctx, report)
	}
	ctx = operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		progress := roomActionJobProgress(update)
		if (progress > 0 || len(update.Worlds) > 0) && strings.TrimSpace(update.Message) != "" {
			_, _ = h.jobs.UpdateProgressDetail(jobID, jobs.ProgressUpdate{
				Progress: progress, Message: update.Message,
				Detail:       &jobs.ProgressDetail{Stage: update.Stage, Worlds: update.Worlds},
				CurrentBytes: update.CurrentBytes, TotalBytes: update.TotalBytes, BytesPerSecond: update.BytesPerSecond,
			})
		}
	})
	return shards.RunWithWorldProgress(ctx, action, targets, runner, report)
}

func roomActionJobProgress(update operationprogress.Update) int {
	percent := update.Percent
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	switch update.Stage {
	case "world.start", "world.restart":
		return min(99, percent)
	case operationprogress.StageStartConfiguration:
		return 2
	case operationprogress.StageStartRouting:
		return 4
	case operationprogress.StageStartMods:
		return 5
	case operationprogress.StageModInspect:
		return 7
	case operationprogress.StageModCache:
		return 10 + percent*45/100
	case operationprogress.StageModPrepare:
		return 58
	case operationprogress.StageModPublish:
		return 63
	case operationprogress.StageModComplete:
		return 68
	case operationprogress.StageModDone:
		return 70
	default:
		return 0
	}
}

func (h *RoomHandler) batchAction(c *gin.Context) {
	action := shards.Action(strings.ToLower(strings.TrimSpace(c.Param("action"))))
	var request batchRoomActionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的批量操作配置", nil)
		return
	}
	selections := make([]shards.BatchRoomSelection, 0, len(request.Rooms))
	allowCapacityRisk := request.AllowCapacityRisk
	if !allowCapacityRisk && len(request.Rooms) > 0 {
		allowCapacityRisk = true
		for _, room := range request.Rooms {
			if !room.AllowCapacityRisk {
				allowCapacityRisk = false
				break
			}
		}
	}
	for _, room := range request.Rooms {
		if action == shards.ActionStart || action == shards.ActionRestart {
			if _, err := h.operations.RequireRuntimeSelection(c.Request.Context(), room.RoomID, room.WorldIDs, room.RuntimeMode, room.RuntimeVersion); err != nil {
				roomFailure(c, err)
				return
			}
		}
		selections = append(selections, shards.BatchRoomSelection{
			RoomID: room.RoomID, WorldIDs: room.WorldIDs, RuntimeMode: room.RuntimeMode,
		})
	}
	targets, runner, err := h.operations.PlanBatch(action, selections)
	if err != nil {
		roomFailure(c, err)
		return
	}
	if err := h.operations.RequireBatchCapacityConfirmation(c.Request.Context(), action, selections, allowCapacityRisk); err != nil {
		roomFailure(c, err)
		return
	}
	requestID := RequestID(c)
	job, err := h.jobs.SubmitFactory("rooms."+string(action), "", "", targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			if h.audit != nil {
				for _, selection := range selections {
					if auditErr := h.audit.RecordAction(runtimeaudit.ActionRequest{
						RoomID: selection.RoomID, WorldIDs: selection.WorldIDs, Action: string(action), Source: runtimeaudit.SourceAPI,
						JobID: job.ID, RequestID: requestID,
					}); auditErr != nil {
						log.Printf("[RuntimeAudit] record batch API action room=%s action=%s: %v", selection.RoomID, action, auditErr)
					}
				}
			}
			ctx = shards.WithOperationAudit(ctx, shards.OperationAuditMetadata{
				JobID: job.ID, RequestID: requestID, Source: string(runtimeaudit.SourceAPI),
			})
			return h.runActionJob(ctx, job.ID, action, targets, runner, report)
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建批量运行任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func roomFailure(c *gin.Context, err error) {
	var validation *rooms.ValidationError
	var capacityRisk *shards.CapacityRiskError
	var batchCapacityRisk *shards.BatchCapacityRiskError
	var runtimeMode *shards.RuntimeModeError
	var runtimeVersion *shards.RuntimeVersionError
	switch {
	case errors.As(err, &validation):
		Failure(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "房间配置校验失败", validation.Fields)
	case errors.As(err, &capacityRisk):
		Failure(c, http.StatusUnprocessableEntity, "CAPACITY_RISK_CONFIRMATION_REQUIRED", "启动后可能超过建议核心容量，请确认卡顿风险", capacityRisk.Preview)
	case errors.As(err, &batchCapacityRisk):
		Failure(c, http.StatusUnprocessableEntity, "CAPACITY_RISK_CONFIRMATION_REQUIRED", "批量启动后可能超过建议核心容量，请确认卡顿风险", batchCapacityRisk.Preview)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间或世界标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	case errors.Is(err, rooms.ErrRecoveryNotFound):
		Failure(c, http.StatusNotFound, "RECOVERY_NOT_FOUND", "回收项不存在", nil)
	case errors.Is(err, rooms.ErrRoomExists):
		Failure(c, http.StatusConflict, "ROOM_EXISTS", "房间目录已经存在", nil)
	case errors.Is(err, rooms.ErrWorldExists):
		Failure(c, http.StatusConflict, "WORLD_EXISTS", "世界目录已经存在", nil)
	case errors.Is(err, rooms.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可操作，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, rooms.ErrConfirmation):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名称确认删除", nil)
	case errors.Is(err, rooms.ErrRecoveryConfirmation):
		Failure(c, http.StatusUnprocessableEntity, "RECOVERY_CONFIRMATION_REQUIRED", "请输入完整回收项名称确认永久清理", nil)
	case errors.Is(err, rooms.ErrWorldRunning):
		Failure(c, http.StatusConflict, "WORLD_RUNNING", "请先停止相关世界再执行此操作", nil)
	case errors.Is(err, rooms.ErrInvalidRoom), errors.Is(err, rooms.ErrInvalidWorld):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_DST_CONFIG", "DST 房间或世界配置不完整", nil)
	case errors.Is(err, shards.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可操作，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, shards.ErrNoWorlds):
		Failure(c, http.StatusUnprocessableEntity, "NO_WORLDS", "房间中没有可控制的世界", nil)
	case errors.Is(err, shards.ErrNoRooms):
		Failure(c, http.StatusUnprocessableEntity, "NO_ROOMS", "请至少选择一个房间", nil)
	case errors.Is(err, shards.ErrInvalidBatch):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_BATCH_SELECTION", "批量操作中的房间不能为空或重复", nil)
	case errors.As(err, &runtimeMode):
		Failure(c, http.StatusUnprocessableEntity, "RUNTIME_MODE_UNAVAILABLE", runtimeMode.Error(), runtimeMode.Availability)
	case errors.As(err, &runtimeVersion):
		Failure(c, http.StatusUnprocessableEntity, "RUNTIME_VERSION_UNAVAILABLE", runtimeVersion.Error(), runtimeVersion.Availability)
	case errors.Is(err, shards.ErrInvalidRuntimeMode):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_MODE", "Lua 运行时模式无效", nil)
	case errors.Is(err, shards.ErrInvalidRuntimeVersion):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_VERSION", "Lua 运行时版本无效", nil)
	case errors.Is(err, shards.ErrUnknownAction):
		Failure(c, http.StatusNotFound, "ACTION_NOT_FOUND", "不支持该房间操作", nil)
	case errors.Is(err, shards.ErrUnsafeName):
		Failure(c, http.StatusUnprocessableEntity, "UNSUPPORTED_DIRECTORY_NAME", "目录名称不符合运行时安全规则，请重命名后重试", nil)
	case errors.Is(err, runtimedriver.ErrRoomRecoveryMultiTarget):
		Failure(c, http.StatusConflict, "ROOM_RECOVERY_MULTI_TARGET", "房间分布在多个运行实例，当前不能整体移入回收站", nil)
	case errors.Is(err, runtimedriver.ErrCapabilityMissing):
		Failure(c, http.StatusConflict, "ROOM_RECOVERY_UNAVAILABLE", "运行节点版本不支持房间回收，请先升级 Agent", nil)
	default:
		Failure(c, http.StatusInternalServerError, "ROOM_OPERATION_FAILED", "房间操作失败", nil)
	}
}
