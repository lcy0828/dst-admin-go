package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/shards"

	"github.com/gin-gonic/gin"
)

type RoomHandler struct {
	rooms      *rooms.Service
	operations *shards.Operations
	jobs       *jobs.Service
}

func NewRoomHandler(roomService *rooms.Service, operations *shards.Operations, jobService *jobs.Service) *RoomHandler {
	return &RoomHandler{rooms: roomService, operations: operations, jobs: jobService}
}

func (h *RoomHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/rooms")
	group.GET("", h.list)
	group.POST("", h.create)
	group.GET("/recovery", h.roomRecoveries)
	group.POST("/recovery/:recoveryName/actions/restore", h.restoreRoom)
	group.DELETE("/recovery/:recoveryName", h.purgeRoomRecovery)
	group.GET("/:roomId", h.get)
	group.DELETE("/:roomId", h.deleteRoom)
	group.POST("/:roomId/adopt", h.adopt)
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
	result, err := h.rooms.DeleteRoom(c.Param("roomId"), request)
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, result)
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
		status, statusErr := h.operations.Status(c.Request.Context(), room.DirectoryName, world.DirectoryName)
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

func (h *RoomHandler) adopt(c *gin.Context) {
	result, err := h.rooms.Adopt(c.Param("roomId"))
	if err != nil {
		roomFailure(c, err)
		return
	}
	Success(c, http.StatusOK, result)
}

type worldState struct {
	rooms.World
	Status           string `json:"status"`
	ControlAvailable bool   `json:"controlAvailable"`
	StatusMessage    string `json:"statusMessage,omitempty"`
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
		state := worldState{World: world, Status: "unknown", ControlAvailable: room.Managed}
		if !room.Managed {
			state.StatusMessage = "接管房间后可执行运行操作"
			result = append(result, state)
			continue
		}
		status, statusErr := h.operations.Status(c.Request.Context(), room.DirectoryName, world.DirectoryName)
		if statusErr != nil {
			state.StatusMessage = statusErr.Error()
			result = append(result, state)
			continue
		}
		state.Status = string(status.State)
		state.StatusMessage = status.Message
		result = append(result, state)
	}
	Success(c, http.StatusOK, gin.H{"items": result, "total": len(result)})
}

type roomActionRequest struct {
	WorldIDs []string `json:"worldIds"`
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
	targets, runner, err := h.operations.Plan(action, c.Param("roomId"), request.WorldIDs)
	if err != nil {
		roomFailure(c, err)
		return
	}
	worldID := ""
	if len(request.WorldIDs) == 1 {
		worldID = request.WorldIDs[0]
	}
	job, err := h.jobs.Submit("room."+string(action), c.Param("roomId"), worldID, targets, runner)
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建运行任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func roomFailure(c *gin.Context, err error) {
	var validation *rooms.ValidationError
	switch {
	case errors.As(err, &validation):
		Failure(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "房间配置校验失败", validation.Fields)
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
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能修改世界", nil)
	case errors.Is(err, rooms.ErrConfirmation):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名称确认删除", nil)
	case errors.Is(err, rooms.ErrRecoveryConfirmation):
		Failure(c, http.StatusUnprocessableEntity, "RECOVERY_CONFIRMATION_REQUIRED", "请输入完整回收项名称确认永久清理", nil)
	case errors.Is(err, rooms.ErrWorldRunning):
		Failure(c, http.StatusConflict, "WORLD_RUNNING", "请先停止相关世界再执行此操作", nil)
	case errors.Is(err, rooms.ErrInvalidRoom), errors.Is(err, rooms.ErrInvalidWorld):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_DST_CONFIG", "DST 房间或世界配置不完整", nil)
	case errors.Is(err, shards.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "请先接管房间再执行操作", nil)
	case errors.Is(err, shards.ErrNoWorlds):
		Failure(c, http.StatusUnprocessableEntity, "NO_WORLDS", "房间中没有可控制的世界", nil)
	case errors.Is(err, shards.ErrUnknownAction):
		Failure(c, http.StatusNotFound, "ACTION_NOT_FOUND", "不支持该房间操作", nil)
	case errors.Is(err, shards.ErrUnsafeName):
		Failure(c, http.StatusUnprocessableEntity, "UNSUPPORTED_DIRECTORY_NAME", "目录名称不符合 tmux 安全规则，请重命名后再接管", nil)
	default:
		Failure(c, http.StatusInternalServerError, "ROOM_OPERATION_FAILED", "房间操作失败", nil)
	}
}
