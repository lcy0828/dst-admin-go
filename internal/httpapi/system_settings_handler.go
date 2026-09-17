package httpapi

import (
	"errors"
	"net/http"

	"dont/internal/systemsettings"

	"github.com/gin-gonic/gin"
)

type SystemSettingsHandler struct{ service *systemsettings.Service }

func NewSystemSettingsHandler(service *systemsettings.Service) *SystemSettingsHandler {
	return &SystemSettingsHandler{service: service}
}

func (h *SystemSettingsHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/system/settings", h.get)
	v2.POST("/system/settings/preview", h.preview)
	v2.POST("/system/settings/actions/apply", h.apply)
	v2.POST("/system/settings/actions/test-email", h.testEmail)
}

func (h *SystemSettingsHandler) get(c *gin.Context) {
	value, err := h.service.Settings()
	if err != nil {
		systemSettingsFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *SystemSettingsHandler) preview(c *gin.Context) {
	var input systemsettings.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "系统设置请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.Preview(input)
	if err != nil {
		systemSettingsFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *SystemSettingsHandler) apply(c *gin.Context) {
	var input systemsettings.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "系统设置请求不是有效 JSON", nil)
		return
	}
	if whitelist, changed := input.Values["security.ipWhitelist"]; changed && !systemsettings.IPAllowed(whitelist, c.ClientIP()) {
		Failure(c, http.StatusUnprocessableEntity, "IP_WHITELIST_LOCKOUT", "IP 白名单必须包含当前访问地址", map[string]string{"security.ipWhitelist": c.ClientIP()})
		return
	}
	value, err := h.service.ApplyContext(c.Request.Context(), input)
	if err != nil {
		systemSettingsFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *SystemSettingsHandler) testEmail(c *gin.Context) {
	var input systemsettings.SMTPTestInput
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "SMTP 测试参数无效", nil)
		return
	}
	value, err := h.service.TestSMTP(c.Request.Context(), input)
	if err != nil {
		Failure(c, http.StatusUnprocessableEntity, "SMTP_TEST_FAILED", "SMTP 连接或认证失败", gin.H{"reason": err.Error(), "result": value})
		return
	}
	Success(c, http.StatusOK, value)
}

func systemSettingsFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, systemsettings.ErrRuntimeBusy):
		Failure(c, http.StatusConflict, "SYSTEM_SETTINGS_RUNTIME_BUSY", err.Error(), nil)
	case errors.Is(err, systemsettings.ErrConflict):
		Failure(c, http.StatusConflict, "SYSTEM_SETTINGS_CONFLICT", "系统设置已被其他操作修改，请刷新后重试", nil)
	case errors.Is(err, systemsettings.ErrConfirmationRequired):
		Failure(c, http.StatusUnprocessableEntity, "SYSTEM_SETTINGS_CONFIRMATION_REQUIRED", "请输入 APPLY SYSTEM SETTINGS 确认应用", map[string]string{"confirmation": "确认短语不匹配"})
	case errors.Is(err, systemsettings.ErrInvalidInput):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_SYSTEM_SETTINGS", "系统设置校验失败", nil)
	default:
		Failure(c, http.StatusInternalServerError, "SYSTEM_SETTINGS_FAILED", "系统设置操作失败", nil)
	}
}
