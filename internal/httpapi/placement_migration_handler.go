package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"dont/internal/jobs"
	"dont/internal/placementmigration"
	"dont/internal/rooms"
	"dont/internal/topology"

	"github.com/gin-gonic/gin"
)

type PlacementMigrationRooms interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type PlacementMigrationCoordinator interface {
	Migrate(context.Context, string, string, string) (placementmigration.Result, error)
}

type PlacementMigrationHandler struct {
	coordinator PlacementMigrationCoordinator
	rooms       PlacementMigrationRooms
	jobs        *jobs.Service
}

func NewPlacementMigrationHandler(coordinator PlacementMigrationCoordinator, roomService PlacementMigrationRooms, jobService *jobs.Service) (*PlacementMigrationHandler, error) {
	if coordinator == nil || roomService == nil || jobService == nil {
		return nil, errors.New("placement migration handler dependencies are required")
	}
	return &PlacementMigrationHandler{coordinator: coordinator, rooms: roomService, jobs: jobService}, nil
}

func (h *PlacementMigrationHandler) Register(v2 *gin.RouterGroup) {
	v2.POST("/rooms/:roomId/topology/actions/apply", h.apply)
}

func (h *PlacementMigrationHandler) apply(c *gin.Context) {
	var request struct {
		WorldID          string `json:"worldId" binding:"required"`
		ExpectedRevision string `json:"expectedRevision" binding:"required"`
		Confirmation     string `json:"confirmation" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "迁移请求不是有效 JSON", nil)
		return
	}
	roomID := c.Param("roomId")
	room, err := h.rooms.Room(roomID)
	if err != nil {
		topologyFailure(c, err)
		return
	}
	if strings.TrimSpace(request.Confirmation) != room.Name {
		Failure(c, http.StatusUnprocessableEntity, "MIGRATION_CONFIRMATION_REQUIRED", "请输入房间名称确认迁移", nil)
		return
	}
	world, err := h.rooms.World(room.ID, strings.TrimSpace(request.WorldID))
	if err != nil {
		topologyFailure(c, err)
		return
	}
	expectedRevision := strings.TrimSpace(request.ExpectedRevision)
	job, err := h.jobs.Submit("placement.migrate", room.ID, world.ID, []jobs.TargetSpec{{ID: world.ID, Name: world.Name}}, func(ctx context.Context, report func(jobs.TargetResult)) error {
		result, migrationErr := h.coordinator.Migrate(ctx, room.ID, world.ID, expectedRevision)
		if migrationErr != nil {
			report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: placementMigrationErrorCode(migrationErr), Message: migrationErr.Error()}})
			return migrationErr
		}
		transferLabel := "Controller 兼容中转"
		if result.TransferSource == placementmigration.TransferSourcePeer {
			transferLabel = "运行节点直传"
		}
		message := fmt.Sprintf("分片已迁移至 %s，通过%s传输 %d 字节；源恢复位置：%s", result.TargetTargetID, transferLabel, result.BytesTransferred, result.RecoveryRef)
		if len(result.CleanupWarnings) > 0 {
			message += "；" + strings.Join(result.CleanupWarnings, "；")
		}
		report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusSucceeded, Message: message})
		return nil
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建分片迁移任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func placementMigrationErrorCode(err error) string {
	var conflict *topology.RevisionConflictError
	var execution *topology.ExecutionError
	switch {
	case errors.Is(err, placementmigration.ErrRevisionChanged), errors.As(err, &conflict):
		return "TOPOLOGY_REVISION_CONFLICT"
	case errors.Is(err, placementmigration.ErrShardRunning):
		return "MIGRATION_SHARD_RUNNING"
	case errors.Is(err, placementmigration.ErrRuntimeRestore):
		return "MIGRATION_RUNTIME_RESTORE_FAILED"
	case errors.As(err, &execution) && execution.Code != "":
		return execution.Code
	default:
		return "PLACEMENT_MIGRATION_FAILED"
	}
}
