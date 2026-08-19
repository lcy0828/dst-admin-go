package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"dont/internal/jobs"
	"dont/internal/operationlease"
	"dont/internal/roomprovision"
	"dont/internal/rooms"
	"dont/internal/topology"

	"github.com/gin-gonic/gin"
)

type RoomProvisionService interface {
	List(string) ([]roomprovision.Operation, error)
	Get(string) (roomprovision.Operation, error)
	Provision(context.Context, string, string, string) (roomprovision.Operation, error)
	RecoverOperation(context.Context, string) (roomprovision.Operation, error)
}

type RoomProvisionCatalog interface {
	Room(string) (rooms.Room, error)
}

type RoomProvisionHandler struct {
	service RoomProvisionService
	rooms   RoomProvisionCatalog
	jobs    *jobs.Service
}

func NewRoomProvisionHandler(service RoomProvisionService, roomCatalog RoomProvisionCatalog, jobService *jobs.Service) (*RoomProvisionHandler, error) {
	if service == nil || roomCatalog == nil || jobService == nil {
		return nil, errors.New("room provision handler dependencies are required")
	}
	return &RoomProvisionHandler{service: service, rooms: roomCatalog, jobs: jobService}, nil
}

func (h *RoomProvisionHandler) Register(v2 *gin.RouterGroup) {
	v2.POST("/rooms/:roomId/topology/actions/provision", h.provision)
	v2.GET("/rooms/:roomId/provision-operations", h.list)
	v2.POST("/provision-operations/:provisionOperationId/actions/recover", h.recover)
}

func (h *RoomProvisionHandler) list(c *gin.Context) {
	values, err := h.service.List(c.Param("roomId"))
	if err != nil {
		roomProvisionFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": values, "total": len(values)})
}

func (h *RoomProvisionHandler) provision(c *gin.Context) {
	var request roomprovision.Request
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "配置投放请求不是有效 JSON", nil)
		return
	}
	request.ExpectedRevision = strings.TrimSpace(request.ExpectedRevision)
	room, err := h.rooms.Room(c.Param("roomId"))
	if err != nil {
		roomProvisionFailure(c, err)
		return
	}
	if request.ExpectedRevision == "" {
		Failure(c, http.StatusUnprocessableEntity, "PROVISION_REVISION_REQUIRED", "缺少当前拓扑版本，请刷新后重试", nil)
		return
	}
	if request.Confirmation != room.Name {
		Failure(c, http.StatusUnprocessableEntity, "PROVISION_CONFIRMATION_REQUIRED", "请输入完整房间名确认配置投放", nil)
		return
	}
	job, err := h.jobs.SubmitFactory("room.provision", room.ID, "", []jobs.TargetSpec{{ID: room.ID, Name: "全部计划分片"}}, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			operation, provisionErr := h.service.Provision(ctx, room.ID, request.ExpectedRevision, job.ID)
			if provisionErr != nil {
				report(jobs.TargetResult{TargetID: room.ID, Status: jobs.StatusFailed, Error: roomProvisionJobError(provisionErr)})
				return nil
			}
			report(jobs.TargetResult{
				TargetID: room.ID, Status: jobs.StatusSucceeded,
				Message: fmt.Sprintf("配置已投放到 %d 个计划分片，操作 ID：%s", len(operation.Steps), operation.ID),
			})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建配置投放任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *RoomProvisionHandler) recover(c *gin.Context) {
	operation, err := h.service.Get(c.Param("provisionOperationId"))
	if err != nil {
		roomProvisionFailure(c, err)
		return
	}
	job, err := h.jobs.SubmitFactory("room.provision.recover", operation.RoomID, "", []jobs.TargetSpec{{ID: operation.ID, Name: operation.RoomName}}, func(jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			recovered, recoverErr := h.service.RecoverOperation(ctx, operation.ID)
			if recoverErr != nil {
				report(jobs.TargetResult{TargetID: operation.ID, Status: jobs.StatusFailed, Error: roomProvisionJobError(recoverErr)})
				return nil
			}
			report(jobs.TargetResult{TargetID: operation.ID, Status: jobs.StatusSucceeded, Message: "配置投放恢复完成：" + string(recovered.Status)})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建配置投放恢复任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func roomProvisionJobError(err error) *jobs.Error {
	return &jobs.Error{Code: roomProvisionErrorCode(err), Message: err.Error()}
}

func roomProvisionErrorCode(err error) string {
	var execution *topology.ExecutionError
	switch {
	case errors.As(err, &execution) && execution.Code != "":
		return execution.Code
	case errors.Is(err, roomprovision.ErrTargetExists):
		return "PROVISION_TARGET_EXISTS"
	case errors.Is(err, roomprovision.ErrMigrationNeeded):
		return "PROVISION_MIGRATION_REQUIRED"
	case errors.Is(err, roomprovision.ErrTopologyChanged):
		return "PROVISION_TOPOLOGY_CHANGED"
	case errors.Is(err, roomprovision.ErrTargetNotReady):
		return "PROVISION_TARGET_NOT_READY"
	case errors.Is(err, roomprovision.ErrRecoveryNeeded):
		return "PROVISION_RECOVERY_REQUIRED"
	case errors.Is(err, rooms.ErrWorldRunning):
		return "PROVISION_WORLD_RUNNING"
	case errors.Is(err, operationlease.ErrBusy):
		return "ROOM_OPERATION_BUSY"
	default:
		return "ROOM_PROVISION_FAILED"
	}
}

func roomProvisionFailure(c *gin.Context, err error) {
	var execution *topology.ExecutionError
	switch {
	case errors.Is(err, roomprovision.ErrNotFound):
		Failure(c, http.StatusNotFound, "PROVISION_OPERATION_NOT_FOUND", "配置投放操作不存在", nil)
	case errors.Is(err, rooms.ErrRoomNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间不存在", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间或操作标识无效", nil)
	case errors.As(err, &execution):
		Failure(c, http.StatusConflict, execution.Code, execution.Message, nil)
	case errors.Is(err, roomprovision.ErrInvalidInput):
		Failure(c, http.StatusUnprocessableEntity, "ROOM_PROVISION_INVALID", "配置投放请求无效", nil)
	case errors.Is(err, roomprovision.ErrTargetExists), errors.Is(err, roomprovision.ErrMigrationNeeded),
		errors.Is(err, roomprovision.ErrTopologyChanged), errors.Is(err, roomprovision.ErrTargetNotReady),
		errors.Is(err, roomprovision.ErrRecoveryNeeded), errors.Is(err, rooms.ErrWorldRunning), errors.Is(err, operationlease.ErrBusy):
		Failure(c, http.StatusConflict, roomProvisionErrorCode(err), err.Error(), nil)
	default:
		Failure(c, http.StatusInternalServerError, "ROOM_PROVISION_FAILED", "配置投放操作失败", nil)
	}
}
