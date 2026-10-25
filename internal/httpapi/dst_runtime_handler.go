package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"dont/internal/agents"
	"dont/internal/dstruntime"
	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/shared"

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

type distributedDSTRuntimeBridge interface {
	Health(context.Context, string, string) (dstruntime.Health, error)
	IsLocalPlacement(string, string) (bool, error)
}

type DSTRuntimeCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type DSTRuntimeProcess interface {
	IsRunning(context.Context, string, string) (bool, error)
}

type identifiedDSTRuntimeProcess interface {
	StatusFor(context.Context, string, string) (shards.RuntimeStatus, error)
}

type DSTRuntimeUpdater interface {
	EnsureWorld(context.Context, string, string, bool) (shared.RuntimeLuaResult, error)
	Report(string, string) (dstruntime.UpdateReport, bool)
}

type DSTRuntimeHandler struct {
	updater DSTRuntimeUpdater
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

func (h *DSTRuntimeHandler) ConfigureUpdater(updater DSTRuntimeUpdater) { h.updater = updater }

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
	appendReport := func(report dstruntime.WorldReport) {
		if report.AutoUpdate != nil && report.AutoUpdate.State == "failed" {
			report.HealthMessage = "Runtime 自动更新失败：" + report.AutoUpdate.Message
		}
		items = append(items, report)
	}
	for _, status := range statuses {
		report := dstruntime.WorldReport{WorldStatus: status, HealthState: dstruntime.HealthStateUnavailable}
		if h.updater != nil {
			if update, ok := h.updater.Report(room.ID, status.WorldID); ok {
				report.AutoUpdate = &update
			}
		}
		world, worldErr := h.rooms.World(room.ID, status.WorldID)
		processStatus := shards.RuntimeStatus{State: shards.RuntimeUnknown}
		if worldErr == nil {
			processStatus, worldErr = h.processStatus(c.Request.Context(), room, world)
			report.ProcessRunning = processStatus.State == shards.RuntimeRunning
			report.Paused, report.StartupStage = processStatus.Paused, processStatus.StartupStage
		}
		if worldErr != nil {
			report.HealthMessage = "无法确认分片进程状态：" + worldErr.Error()
			appendReport(report)
			continue
		}
		if bridge, ok := h.bridge.(distributedDSTRuntimeBridge); ok {
			local, localityErr := bridge.IsLocalPlacement(room.ID, status.WorldID)
			if localityErr != nil {
				report.HealthMessage = "无法解析分片执行位置：" + localityErr.Error()
				appendReport(report)
				continue
			}
			if !local {
				installationObserved := false
				if update := report.AutoUpdate; update != nil {
					switch state := dstruntime.InstallState(update.InstallationState); state {
					case dstruntime.InstallStateMissing, dstruntime.InstallStateOutdated, dstruntime.InstallStateInvalid, dstruntime.InstallStateInstalled:
						installationObserved = true
						status.State, status.Version, status.Message = state, update.Version, update.Message
						status.Protocol = dstruntime.ProtocolVersion
						report.WorldStatus = status
						// Health files survive disabling or removing Runtime. The
						// Agent's installation observation takes precedence over
						// those files and same-named paths on the Controller.
						if state != dstruntime.InstallStateInstalled {
							report.HealthMessage = "运行时尚未安装或需要修复"
							appendReport(report)
							continue
						}
						if processStatus.State == shards.RuntimeStopped || processStatus.State == shards.RuntimeFailed {
							report.HealthState, report.HealthMessage = dstruntime.HealthStateStopped, update.Message
							appendReport(report)
							continue
						}
					}
				}
				readHealth := bridge.Health
				if observer, ok := h.bridge.(interface {
					ObserveHealth(context.Context, string, string) (dstruntime.Health, error)
				}); ok {
					readHealth = observer.ObserveHealth
				}
				health, healthErr := readHealth(c.Request.Context(), room.ID, status.WorldID)
				if healthErr != nil {
					if status.State != dstruntime.InstallStateInstalled {
						status.State = dstruntime.InstallStateMissing
						status.Version = ""
					}
					status.Message = "目标 Agent 未提供可用的 Runtime 制品"
					report.WorldStatus = status
					report.HealthMessage = healthErr.Error()
					if installationObserved && processStatus.State == shards.RuntimeStarting {
						report.HealthState, report.HealthMessage = dstruntime.HealthStateStarting, "游戏仍在启动，等待首次采集"
					}
					appendReport(report)
					continue
				}
				if !installationObserved {
					status.State, status.Version, status.Protocol = dstruntime.InstallStateInstalled, health.ProducerVersion, dstruntime.ProtocolVersion
				}
				report.WorldStatus, report.Health = status, &health
				report.HealthState, report.HealthMessage = h.describeHealth(c.Request.Context(), room.ID, status.WorldID, health, processStatus)
				if report.ProcessRunning && status.Version != "" && health.ProducerVersion != status.Version {
					report.HealthState = dstruntime.HealthStateDegraded
					report.HealthMessage = "运行中的版本为 " + health.ProducerVersion + "，已安装版本为 " + status.Version + "；请激活以应用新版本"
				}
				appendReport(report)
				continue
			}
		}
		if status.State != dstruntime.InstallStateInstalled {
			report.HealthMessage = "运行时尚未安装或需要修复"
			appendReport(report)
			continue
		}
		if processStatus.State == shards.RuntimeStarting {
			report.HealthState, report.HealthMessage = dstruntime.HealthStateStarting, "游戏仍在启动，等待首次采集"
			appendReport(report)
			continue
		}
		health, healthErr := h.runtime.Health(room.ID, status.WorldID)
		if healthErr != nil {
			report.HealthMessage = healthErr.Error()
			appendReport(report)
			continue
		}
		report.Health = &health
		report.HealthState, report.HealthMessage = h.describeHealth(c.Request.Context(), room.ID, status.WorldID, health, processStatus)
		if report.ProcessRunning && status.Version != "" && health.ProducerVersion != status.Version {
			report.HealthState = dstruntime.HealthStateDegraded
			report.HealthMessage = "运行中的版本为 " + health.ProducerVersion + "，已安装版本为 " + status.Version + "；请激活以应用新版本"
		}
		appendReport(report)
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (h *DSTRuntimeHandler) installRoom(c *gin.Context) {
	if h.updater != nil {
		statuses, err := h.runtime.StatusRoom(c.Param("roomId"))
		if err != nil {
			dstRuntimeFailure(c, err)
			return
		}
		for i := range statuses {
			value, err := h.updater.EnsureWorld(c.Request.Context(), c.Param("roomId"), statuses[i].WorldID, true)
			if err != nil {
				dstRuntimeFailure(c, err)
				return
			}
			statuses[i].State, statuses[i].Version, statuses[i].Protocol, statuses[i].Changed, statuses[i].Message = dstruntime.InstallState(value.State), value.Version, dstruntime.ProtocolVersion, value.Changed, value.Message
		}
		Success(c, http.StatusOK, gin.H{"items": statuses, "total": len(statuses)})
		return
	}
	if bridge, ok := h.bridge.(distributedDSTRuntimeBridge); ok {
		statuses, err := h.runtime.StatusRoom(c.Param("roomId"))
		if err != nil {
			dstRuntimeFailure(c, err)
			return
		}
		for _, status := range statuses {
			local, localityErr := bridge.IsLocalPlacement(c.Param("roomId"), status.WorldID)
			if localityErr != nil {
				dstRuntimeFailure(c, localityErr)
				return
			}
			if !local {
				Failure(c, http.StatusConflict, "REMOTE_RUNTIME_MUTATION_UNAVAILABLE", "房间包含远程分片；请在拓扑中逐个管理 Runtime，当前版本不会修改控制端同名路径", nil)
				return
			}
		}
	}
	statuses, err := h.runtime.InstallRoom(c.Request.Context(), c.Param("roomId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": statuses, "total": len(statuses)})
}

func (h *DSTRuntimeHandler) installWorld(c *gin.Context) {
	if h.updater != nil {
		roomID, worldID := c.Param("roomId"), c.Param("worldId")
		value, err := h.updater.EnsureWorld(c.Request.Context(), roomID, worldID, true)
		if err != nil {
			dstRuntimeFailure(c, err)
			return
		}
		world, err := h.rooms.World(roomID, worldID)
		if err != nil {
			dstRuntimeFailure(c, err)
			return
		}
		Success(c, http.StatusOK, dstruntime.WorldStatus{RoomID: roomID, WorldID: worldID, WorldName: world.Name, State: dstruntime.InstallState(value.State), Version: value.Version, Protocol: dstruntime.ProtocolVersion, Changed: value.Changed, Message: value.Message})
		return
	}
	if !h.allowLocalRuntimeMutation(c, c.Param("roomId"), c.Param("worldId")) {
		return
	}
	status, err := h.runtime.InstallWorld(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	Success(c, http.StatusOK, status)
}

func (h *DSTRuntimeHandler) backups(c *gin.Context) {
	if !h.allowLocalRuntimeMutation(c, c.Param("roomId"), c.Param("worldId")) {
		return
	}
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
	ctx, release, err := roomops.Acquire(c.Request.Context(), roomID)
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	defer release()
	c.Request = c.Request.WithContext(ctx)
	if !h.allowLocalRuntimeMutation(c, roomID, worldID) {
		return
	}
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
	ctx, release, err := roomops.Acquire(c.Request.Context(), roomID)
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	defer release()
	c.Request = c.Request.WithContext(ctx)
	if !h.allowLocalRuntimeMutation(c, roomID, worldID) {
		return
	}
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

func (h *DSTRuntimeHandler) allowLocalRuntimeMutation(c *gin.Context, roomID, worldID string) bool {
	bridge, ok := h.bridge.(distributedDSTRuntimeBridge)
	if !ok {
		return true
	}
	local, err := bridge.IsLocalPlacement(roomID, worldID)
	if err != nil {
		dstRuntimeFailure(c, err)
		return false
	}
	if !local {
		Failure(c, http.StatusConflict, "REMOTE_RUNTIME_MUTATION_UNAVAILABLE", "该分片运行在远程 Agent；当前操作不会回落到控制端本机路径", nil)
		return false
	}
	return true
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
	status, err := h.processStatus(c.Request.Context(), room, world)
	if err != nil {
		dstRuntimeFailure(c, err)
		return false
	}
	if status.State == shards.RuntimeRunning {
		Failure(c, http.StatusConflict, "WORLD_RUNNING", "请先停止分片再修改运行时", nil)
		return false
	}
	if status.State != shards.RuntimeStopped && status.State != shards.RuntimeFailed {
		dstRuntimeFailure(c, dstruntime.ErrRuntimeUnavailable)
		return false
	}
	if status.SessionExists || strings.HasSuffix(status.Code, "_CONFLICT") {
		dstRuntimeFailure(c, fmt.Errorf("%w: %s %s", dstruntime.ErrRuntimeProcessConflict, status.Code, status.Message))
		return false
	}
	return true
}

func (h *DSTRuntimeHandler) processStatus(ctx context.Context, room rooms.Room, world rooms.World) (shards.RuntimeStatus, error) {
	if runtime, ok := h.process.(identifiedDSTRuntimeProcess); ok {
		return runtime.StatusFor(ctx, room.ID, world.ID)
	}
	running, err := h.process.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
	if err != nil {
		return shards.RuntimeStatus{State: shards.RuntimeUnknown}, err
	}
	if running {
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	}
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}

func (h *DSTRuntimeHandler) describeHealth(ctx context.Context, roomID, worldID string, health dstruntime.Health, process shards.RuntimeStatus) (dstruntime.HealthState, string) {
	currentBoot := false
	if process.State == shards.RuntimeRunning && process.Paused != nil && *process.Paused &&
		health.Running && health.Ready && health.LastError == nil && health.ConsecutiveFailures == 0 && runtimeHealthIsStale(health) {
		reader, ok := h.bridge.(interface {
			HealthMatchesCurrentProcess(context.Context, string, string, dstruntime.Health) (bool, error)
		})
		if !ok {
			return dstruntime.HealthStateUnavailable, "分片已暂停，但尚未确认本次启动的 Runtime 健康数据"
		}
		var err error
		currentBoot, err = reader.HealthMatchesCurrentProcess(ctx, roomID, worldID, health)
		if err != nil {
			return dstruntime.HealthStateUnavailable, "无法确认 Runtime 健康数据是否来自本次启动：" + err.Error()
		}
		if !currentBoot {
			return dstruntime.HealthStateUnavailable, "分片已暂停，但尚未确认本次启动的 Runtime 健康数据"
		}
	}
	state := runtimeHealthState(health, process.State == shards.RuntimeRunning, process.Paused, currentBoot)
	if health.LastError != nil {
		return state, *health.LastError
	}
	if health.ConsecutiveFailures > 0 {
		return state, "Runtime 采集连续失败"
	}
	if state == dstruntime.HealthStateDegraded && runtimeHealthIsStale(health) {
		return state, "Runtime 健康数据已超过 20 秒未更新"
	}
	return state, ""
}

func runtimeHealthState(health dstruntime.Health, processRunning bool, processPaused *bool, currentBoot bool) dstruntime.HealthState {
	if health.LastError != nil || health.ConsecutiveFailures > 0 {
		return dstruntime.HealthStateDegraded
	}
	if !processRunning || !health.Running {
		return dstruntime.HealthStateStopped
	}
	if health.Ready {
		if runtimeHealthIsStale(health) && (processPaused == nil || !*processPaused || !currentBoot) {
			return dstruntime.HealthStateDegraded
		}
		return dstruntime.HealthStateReady
	}
	return dstruntime.HealthStateStarting
}

func runtimeHealthIsStale(health dstruntime.Health) bool {
	return !health.FreshAt(time.Now().UTC())
}

func dstRuntimeFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath), errors.Is(err, dstruntime.ErrUnsafeRuntimePath):
		Failure(c, http.StatusBadRequest, "INVALID_RUNTIME_RESOURCE", "运行时资源标识或路径无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound), errors.Is(err, os.ErrNotExist):
		Failure(c, http.StatusNotFound, "RUNTIME_RESOURCE_NOT_FOUND", "房间、分片或运行时备份不存在", nil)
	case errors.Is(err, rooms.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, dstruntime.ErrManagedBlockInvalid), errors.Is(err, dstruntime.ErrManagedFileChanged):
		Failure(c, http.StatusConflict, "RUNTIME_CONFLICT", "检测到用户修改或损坏的运行时文件，已停止操作", nil)
	case errors.Is(err, dstruntime.ErrRuntimeProcessConflict):
		Failure(c, http.StatusConflict, "RUNTIME_PROCESS_CONFLICT", "分片进程归属不明确，请先处理冲突并停止相关进程，再修改 Runtime", gin.H{"reason": err.Error()})
	case errors.Is(err, operationlease.ErrBusy), errors.Is(err, dstruntime.ErrRuntimeRefreshDeferred):
		Failure(c, http.StatusConflict, "RUNTIME_MAINTENANCE_DEFERRED", "当前房间或 Runtime 操作尚未完成，请稍后重试", gin.H{"reason": err.Error()})
	case errors.Is(err, agents.ErrUnsupportedAction):
		Failure(c, http.StatusConflict, "RUNTIME_AGENT_UPGRADE_REQUIRED", "请先升级目标 Agent，再重试 Runtime 维护", gin.H{"reason": err.Error()})
	case errors.Is(err, dstruntime.ErrRuntimeRequestInvalid):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_REQUEST", "运行诊断参数无效", nil)
	case errors.Is(err, dstruntime.ErrRuntimeCommandUnconfirmed):
		Failure(c, http.StatusGatewayTimeout, "COMMAND_OUTCOME_UNKNOWN", "命令已发送，但未在限定时间内收到确认；可能已执行，不会自动重放", gin.H{"mayHaveExecuted": true, "reason": err.Error()})
	case errors.Is(err, dstruntime.ErrRuntimeResultAbsent):
		Failure(c, http.StatusNotFound, "RUNTIME_RESULT_NOT_FOUND", "运行时尚未产生对应结果", nil)
	case errors.Is(err, dstruntime.ErrRuntimeUnavailable), errors.Is(err, dstruntime.ErrRuntimeNotInstalled):
		Failure(c, http.StatusConflict, "RUNTIME_UNAVAILABLE", "目标分片运行时尚未就绪", nil)
	case errors.Is(err, dstruntime.ErrRuntimeActivation):
		Failure(c, http.StatusConflict, "RUNTIME_ACTIVATION_FAILED", "运行时未能在限定时间内进入健康状态；分片未重启，也不会自动重试", nil)
	case errors.Is(err, dstruntime.ErrRuntimeResultStale), errors.Is(err, dstruntime.ErrRuntimeResultInvalid):
		Failure(c, http.StatusConflict, "RUNTIME_RESULT_INVALID", "运行时结果已过期或损坏", nil)
	default:
		Failure(c, http.StatusInternalServerError, "RUNTIME_OPERATION_FAILED", "DST Admin 运行时操作失败", gin.H{"reason": err.Error()})
	}
}
