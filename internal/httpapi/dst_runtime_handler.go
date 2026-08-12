package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type DSTRuntimeManager interface {
	StatusRoom(string) ([]dstruntime.WorldStatus, error)
	Health(string, string) (dstruntime.Health, error)
	InstallRoom(context.Context, string) ([]dstruntime.WorldStatus, error)
	InstallWorld(context.Context, string, string) (dstruntime.WorldStatus, error)
	UninstallWorld(context.Context, string, string) (dstruntime.WorldStatus, error)
	Backups(string, string) ([]dstruntime.Backup, error)
	RollbackWorld(context.Context, string, string, string) (dstruntime.WorldStatus, error)
}

type DSTRuntimeBridge interface {
	Activate(context.Context, string, string) (dstruntime.LifecycleResult, error)
	Reload(context.Context, string, string) (dstruntime.LifecycleResult, error)
	ReadEvents(context.Context, string, string) (dstruntime.EventBatch, error)
	LatestDiagnostic(context.Context, string, string) (dstruntime.DiagnosticReport, error)
	CaptureDiagnostic(context.Context, string, string, dstruntime.DiagnosticRequest) (dstruntime.DiagnosticReport, error)
}

type DSTRuntimeCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type DSTRuntimeProcess interface {
	IsRunning(context.Context, string, string) (bool, error)
}

type DSTRuntimeHandler struct {
	runtime DSTRuntimeManager
	rooms   DSTRuntimeCatalog
	process DSTRuntimeProcess
	bridge  DSTRuntimeBridge
}

type runtimeConfirmationRequest struct {
	Confirmation string `json:"confirmation"`
	BackupID     string `json:"backupId"`
}

type diagnosticRequest struct {
	Profile         string `json:"profile"`
	Prefab          string `json:"prefab"`
	DurationSeconds int    `json:"durationSeconds"`
	SampleLimit     int    `json:"sampleLimit"`
}

func NewDSTRuntimeHandler(runtime DSTRuntimeManager, catalog DSTRuntimeCatalog, process DSTRuntimeProcess, bridges ...DSTRuntimeBridge) *DSTRuntimeHandler {
	handler := &DSTRuntimeHandler{runtime: runtime, rooms: catalog, process: process}
	if len(bridges) > 0 {
		handler.bridge = bridges[0]
	}
	return handler
}

func (h *DSTRuntimeHandler) Register(v2 *gin.RouterGroup) {
	room := v2.Group("/rooms/:roomId")
	room.GET("/runtime", h.status)
	room.POST("/runtime/actions/install", h.installRoom)
	room.POST("/worlds/:worldId/runtime/actions/install", h.installWorld)
	room.POST("/worlds/:worldId/runtime/actions/activate", h.activate)
	room.POST("/worlds/:worldId/runtime/actions/reload", h.reload)
	room.GET("/worlds/:worldId/runtime/backups", h.backups)
	room.GET("/worlds/:worldId/runtime/events", h.events)
	room.GET("/worlds/:worldId/runtime/diagnostics/latest", h.latestDiagnostic)
	room.POST("/worlds/:worldId/runtime/diagnostics", h.captureDiagnostic)
	room.POST("/worlds/:worldId/runtime/actions/rollback", h.rollback)
	room.DELETE("/worlds/:worldId/runtime", h.uninstall)
}

func (h *DSTRuntimeHandler) activate(c *gin.Context) {
	if h.bridge == nil {
		dstRuntimeFailure(c, dstruntime.ErrRuntimeUnavailable)
		return
	}
	value, err := h.bridge.Activate(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *DSTRuntimeHandler) reload(c *gin.Context) {
	if h.bridge == nil {
		dstRuntimeFailure(c, dstruntime.ErrRuntimeUnavailable)
		return
	}
	value, err := h.bridge.Reload(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *DSTRuntimeHandler) events(c *gin.Context) {
	if h.bridge == nil {
		dstRuntimeFailure(c, dstruntime.ErrRuntimeUnavailable)
		return
	}
	value, err := h.bridge.ReadEvents(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *DSTRuntimeHandler) latestDiagnostic(c *gin.Context) {
	if h.bridge == nil {
		dstRuntimeFailure(c, dstruntime.ErrRuntimeUnavailable)
		return
	}
	value, err := h.bridge.LatestDiagnostic(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *DSTRuntimeHandler) captureDiagnostic(c *gin.Context) {
	if h.bridge == nil {
		dstRuntimeFailure(c, dstruntime.ErrRuntimeUnavailable)
		return
	}
	var input diagnosticRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的运行诊断", nil)
		return
	}
	value, err := h.bridge.CaptureDiagnostic(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), dstruntime.DiagnosticRequest{
		RequestID: uuid.NewString(), Profile: strings.TrimSpace(input.Profile), Prefab: strings.TrimSpace(input.Prefab),
		DurationSeconds: input.DurationSeconds, SampleLimit: input.SampleLimit,
	})
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *DSTRuntimeHandler) status(c *gin.Context) {
	roomID := c.Param("roomId")
	room, err := h.rooms.Room(roomID)
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	statuses, err := h.runtime.StatusRoom(room.ID)
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	items := make([]dstruntime.WorldReport, 0, len(statuses))
	for _, status := range statuses {
		report := dstruntime.WorldReport{WorldStatus: status, HealthState: dstruntime.HealthStateUnavailable}
		world, worldErr := h.rooms.World(room.ID, status.WorldID)
		if worldErr == nil {
			report.ProcessRunning, worldErr = h.process.IsRunning(c.Request.Context(), room.DirectoryName, world.DirectoryName)
		}
		if worldErr != nil {
			report.HealthMessage = "无法确认分片进程状态：" + worldErr.Error()
			items = append(items, report)
			continue
		}
		if status.State != dstruntime.InstallStateInstalled {
			report.HealthMessage = "运行时尚未安装或需要修复"
			items = append(items, report)
			continue
		}
		health, healthErr := h.runtime.Health(room.ID, status.WorldID)
		if healthErr != nil {
			report.HealthMessage = healthErr.Error()
			items = append(items, report)
			continue
		}
		report.Health = &health
		report.HealthState = runtimeHealthState(health, report.ProcessRunning)
		if report.ProcessRunning && status.Version != "" && health.ProducerVersion != status.Version {
			report.HealthState = dstruntime.HealthStateDegraded
			report.HealthMessage = "运行中的版本为 " + health.ProducerVersion + "，已安装版本为 " + status.Version + "；请激活以应用新版本"
		} else if health.LastError != nil {
			report.HealthMessage = *health.LastError
		}
		items = append(items, report)
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *DSTRuntimeHandler) installRoom(c *gin.Context) {
	statuses, err := h.runtime.InstallRoom(c.Request.Context(), c.Param("roomId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": statuses, "total": len(statuses)})
}

func (h *DSTRuntimeHandler) installWorld(c *gin.Context) {
	status, err := h.runtime.InstallWorld(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, status)
}

func (h *DSTRuntimeHandler) backups(c *gin.Context) {
	values, err := h.runtime.Backups(c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": values, "total": len(values)})
}

func (h *DSTRuntimeHandler) rollback(c *gin.Context) {
	var request runtimeConfirmationRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的运行时回滚操作", nil)
		return
	}
	roomID, worldID := c.Param("roomId"), c.Param("worldId")
	if !h.allowStoppedMutation(c, roomID, worldID, request.Confirmation) {
		return
	}
	status, err := h.runtime.RollbackWorld(c.Request.Context(), roomID, worldID, strings.TrimSpace(request.BackupID))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, status)
}

func (h *DSTRuntimeHandler) uninstall(c *gin.Context) {
	var request runtimeConfirmationRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的运行时卸载操作", nil)
		return
	}
	roomID, worldID := c.Param("roomId"), c.Param("worldId")
	if !h.allowStoppedMutation(c, roomID, worldID, request.Confirmation) {
		return
	}
	status, err := h.runtime.UninstallWorld(c.Request.Context(), roomID, worldID)
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, status)
}

func (h *DSTRuntimeHandler) allowStoppedMutation(c *gin.Context, roomID, worldID, confirmation string) bool {
	room, err := h.rooms.Room(roomID)
	if err != nil {
		dstRuntimeFailure(c, err)
		return false
	}
	if confirmation != room.Name {
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入房间名称确认操作", map[string]string{"confirmation": "确认内容不匹配"})
		return false
	}
	world, err := h.rooms.World(room.ID, worldID)
	if err != nil {
		dstRuntimeFailure(c, err)
		return false
	}
	running, err := h.process.IsRunning(c.Request.Context(), room.DirectoryName, world.DirectoryName)
	if err != nil {
		dstRuntimeFailure(c, err)
		return false
	}
	if running {
		Failure(c, http.StatusConflict, "WORLD_RUNNING", "请先停止分片再修改运行时", nil)
		return false
	}
	return true
}

func runtimeHealthState(health dstruntime.Health, processRunning bool) dstruntime.HealthState {
	if health.LastError != nil || health.ConsecutiveFailures > 0 {
		return dstruntime.HealthStateDegraded
	}
	if !processRunning || !health.Running {
		return dstruntime.HealthStateStopped
	}
	if health.Ready {
		if !health.ReadAt.IsZero() && time.Since(health.ReadAt) > 20*time.Second {
			return dstruntime.HealthStateDegraded
		}
		return dstruntime.HealthStateReady
	}
	return dstruntime.HealthStateStarting
}

func dstRuntimeFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath), errors.Is(err, dstruntime.ErrUnsafeRuntimePath):
		Failure(c, http.StatusBadRequest, "INVALID_RUNTIME_RESOURCE", "运行时资源标识或路径无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound), errors.Is(err, os.ErrNotExist):
		Failure(c, http.StatusNotFound, "RUNTIME_RESOURCE_NOT_FOUND", "房间、分片或运行时备份不存在", nil)
	case errors.Is(err, rooms.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能管理运行时", nil)
	case errors.Is(err, dstruntime.ErrManagedBlockInvalid), errors.Is(err, dstruntime.ErrManagedFileChanged):
		Failure(c, http.StatusConflict, "RUNTIME_CONFLICT", "检测到用户修改或损坏的运行时文件，已停止操作", nil)
	case errors.Is(err, dstruntime.ErrRuntimeRequestInvalid):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_REQUEST", "运行诊断参数无效", nil)
	case errors.Is(err, dstruntime.ErrRuntimeResultAbsent):
		Failure(c, http.StatusNotFound, "RUNTIME_RESULT_NOT_FOUND", "运行时尚未产生对应结果", nil)
	case errors.Is(err, dstruntime.ErrRuntimeUnavailable), errors.Is(err, dstruntime.ErrRuntimeNotInstalled):
		Failure(c, http.StatusConflict, "RUNTIME_UNAVAILABLE", "目标分片运行时尚未就绪", nil)
	case errors.Is(err, dstruntime.ErrRuntimeActivation):
		Failure(c, http.StatusConflict, "RUNTIME_ACTIVATION_FAILED", "运行时未能在限定时间内进入健康状态；分片未重启，也不会自动重试", nil)
	case errors.Is(err, dstruntime.ErrRuntimeResultStale), errors.Is(err, dstruntime.ErrRuntimeResultInvalid):
		Failure(c, http.StatusConflict, "RUNTIME_RESULT_INVALID", "运行时结果已过期或损坏", nil)
	default:
		Failure(c, http.StatusInternalServerError, "RUNTIME_OPERATION_FAILED", "DST Admin 运行时操作失败", nil)
	}
}
