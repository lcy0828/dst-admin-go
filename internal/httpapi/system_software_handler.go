package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"dont/internal/softwareupdate"
	"dont/internal/systemsettings"

	"github.com/gin-gonic/gin"
)

type SystemSoftwareHandler struct{ service *softwareupdate.Service }

func NewSystemSoftwareHandler(service *softwareupdate.Service) *SystemSoftwareHandler {
	return &SystemSoftwareHandler{service: service}
}

func (h *SystemSoftwareHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/system/software", h.status)
	v2.GET("/system/software/check", h.check)
	v2.POST("/system/software/actions/update", h.update)
	v2.POST("/system/software/actions/apply", h.apply)
}

func (h *SystemSoftwareHandler) status(c *gin.Context) {
	value, err := h.service.Snapshot()
	if err != nil {
		softwareFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *SystemSoftwareHandler) check(c *gin.Context) {
	force, err := strconv.ParseBool(c.DefaultQuery("force", "false"))
	if err != nil {
		softwareFailure(c, softwareupdate.ErrInvalid)
		return
	}
	value, err := h.service.Check(c.Request.Context(), force, c.DefaultQuery("source", "auto"))
	if err != nil {
		softwareFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *SystemSoftwareHandler) update(c *gin.Context) {
	var input struct {
		Version      string `json:"version"`
		Source       string `json:"source"`
		Confirmation string `json:"confirmation"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || input.Confirmation != input.Version {
		softwareFailure(c, softwareupdate.ErrInvalid)
		return
	}
	if input.Source == "" {
		input.Source = "auto"
	}
	value, err := h.service.Start(input.Version, input.Source)
	if err != nil {
		softwareFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, value)
}

func (h *SystemSoftwareHandler) apply(c *gin.Context) {
	var input struct {
		OperationID  string `json:"operationId"`
		Confirmation string `json:"confirmation"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || input.Confirmation != "APPLY SOFTWARE UPDATE" {
		softwareFailure(c, softwareupdate.ErrInvalid)
		return
	}
	value, resume, err := h.service.PrepareApply(c.Request.Context(), input.OperationID)
	if err != nil {
		softwareFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, value)
	h.service.NotifyApply(resume)
}

func softwareFailure(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "SOFTWARE_UPDATE_FAILED", "管理服务更新失败"
	switch {
	case errors.Is(err, softwareupdate.ErrBusy), errors.Is(err, systemsettings.ErrRuntimeBusy):
		status, code, message = http.StatusConflict, "SOFTWARE_UPDATE_BUSY", "请等待后台任务和配置操作完成后再更新管理服务"
	case errors.Is(err, softwareupdate.ErrUnsupported):
		status, code, message = http.StatusUnprocessableEntity, "SOFTWARE_UPDATE_UNSUPPORTED", "当前部署或发布包不支持页面更新"
	case errors.Is(err, softwareupdate.ErrNotPrepared):
		status, code, message = http.StatusConflict, "SOFTWARE_UPDATE_NOT_PREPARED", "没有可应用的已校验更新，请重新检查"
	case errors.Is(err, softwareupdate.ErrReleaseChanged):
		status, code, message = http.StatusConflict, "SOFTWARE_RELEASE_CHANGED", "新版已变化，请重新检查后更新"
	case errors.Is(err, softwareupdate.ErrInvalid):
		status, code, message = http.StatusBadRequest, "INVALID_SOFTWARE_UPDATE", "版本、确认信息或更新包无效"
	}
	Failure(c, status, code, message, gin.H{"reason": err.Error()})
}
