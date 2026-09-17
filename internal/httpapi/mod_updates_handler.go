package httpapi

import (
	"errors"
	"net/http"

	"dont/internal/jobs"
	"dont/internal/modupdates"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type ModUpdateService interface {
	Overview(string) (modupdates.Overview, error)
	UpdatePolicy(string, modupdates.PolicyInput) (modupdates.Overview, error)
	SubmitCheck(string) (jobs.Job, error)
	SubmitApplyWhenEmpty(string) (jobs.Job, error)
	SubmitApplyNow(string) (jobs.Job, error)
}

type ModUpdateHandler struct{ service ModUpdateService }

func NewModUpdateHandler(service ModUpdateService) (*ModUpdateHandler, error) {
	if service == nil {
		return nil, errors.New("Mod update handler service is required")
	}
	return &ModUpdateHandler{service: service}, nil
}

func (h *ModUpdateHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/rooms/:roomId/mod-update", h.overview)
	v2.PUT("/rooms/:roomId/mod-update/policy", h.updatePolicy)
	v2.POST("/rooms/:roomId/mod-update/actions/check", h.check)
	v2.POST("/rooms/:roomId/mod-update/actions/apply-when-empty", h.applyWhenEmpty)
	v2.POST("/rooms/:roomId/mod-update/actions/apply", h.applyNow)
}

func (h *ModUpdateHandler) overview(c *gin.Context) {
	value, err := h.service.Overview(c.Param("roomId"))
	if err != nil {
		modUpdateFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModUpdateHandler) updatePolicy(c *gin.Context) {
	var input modupdates.PolicyInput
	if !bindModJSON(c, &input) {
		return
	}
	value, err := h.service.UpdatePolicy(c.Param("roomId"), input)
	if err != nil {
		modUpdateFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModUpdateHandler) check(c *gin.Context) {
	job, err := h.service.SubmitCheck(c.Param("roomId"))
	if err != nil {
		modUpdateFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *ModUpdateHandler) applyWhenEmpty(c *gin.Context) {
	job, err := h.service.SubmitApplyWhenEmpty(c.Param("roomId"))
	if err != nil {
		modUpdateFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *ModUpdateHandler) applyNow(c *gin.Context) {
	job, err := h.service.SubmitApplyNow(c.Param("roomId"))
	if err != nil {
		modUpdateFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func modUpdateFailure(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "MOD_UPDATE_FAILED", "模组更新操作失败"
	switch {
	case errors.Is(err, rooms.ErrRoomNotFound):
		status, code, message = http.StatusNotFound, "ROOM_NOT_FOUND", "房间不存在"
	case errors.Is(err, modupdates.ErrInvalidInput):
		status, code, message = http.StatusUnprocessableEntity, "INVALID_MOD_UPDATE_POLICY", "模组更新策略无效"
	case errors.Is(err, modupdates.ErrRevisionConflict):
		status, code, message = http.StatusConflict, "MOD_UPDATE_POLICY_CHANGED", "模组更新策略已变化，请重新加载"
	case errors.Is(err, modupdates.ErrBusy):
		status, code, message = http.StatusConflict, "MOD_UPDATE_BUSY", "该房间已有模组更新任务正在执行"
	}
	Failure(c, status, code, message, gin.H{"reason": err.Error()})
}
