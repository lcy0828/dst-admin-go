package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"dont/internal/notifications"
	"github.com/gin-gonic/gin"
)

type NotificationsHandler struct{ service *notifications.Service }

func NewNotificationsHandler(service *notifications.Service) *NotificationsHandler {
	return &NotificationsHandler{service: service}
}

func (h *NotificationsHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/system/notifications/channels", h.list)
	v2.POST("/system/notifications/channels", h.create)
	v2.PUT("/system/notifications/channels/:channelId", h.update)
	v2.DELETE("/system/notifications/channels/:channelId", h.delete)
	v2.POST("/system/notifications/channels/:channelId/actions/test", h.test)
	v2.GET("/system/notifications/deliveries", h.history)
	v2.GET("/system/notifications/events", h.events)
	v2.POST("/system/notifications/deliveries/:deliveryId/actions/retry", h.retry)
	v2.POST("/system/notifications/actions/send", h.send)
}

func (h *NotificationsHandler) events(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil {
		notificationFailure(c, notifications.ErrInvalid)
		return
	}
	value, err := h.service.Feed(c.Query("after"), c.Query("kinds"), limit)
	if err != nil {
		notificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *NotificationsHandler) list(c *gin.Context) {
	items, err := h.service.Channels()
	if err != nil {
		notificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, gin.H{"items": items, "events": notifications.EventKinds})
}

func (h *NotificationsHandler) create(c *gin.Context) { h.save(c, "") }
func (h *NotificationsHandler) update(c *gin.Context) { h.save(c, c.Param("channelId")) }
func (h *NotificationsHandler) save(c *gin.Context, id string) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var input notifications.ChannelInput
	if err := c.ShouldBindJSON(&input); err != nil {
		notificationFailure(c, notifications.ErrInvalid)
		return
	}
	value, err := h.service.Save(id, input)
	if err != nil {
		notificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *NotificationsHandler) delete(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	var input struct {
		Revision string `json:"revision"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		notificationFailure(c, notifications.ErrInvalid)
		return
	}
	if err := h.service.Delete(c.Param("channelId"), input.Revision); err != nil {
		notificationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *NotificationsHandler) test(c *gin.Context) {
	items, err := h.service.Test(c.Param("channelId"))
	if err != nil {
		notificationFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, gin.H{"items": items})
}

func (h *NotificationsHandler) history(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	items, total, err := h.service.History(limit, offset)
	if err != nil {
		notificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, gin.H{"items": items, "total": total})
}

func (h *NotificationsHandler) retry(c *gin.Context) {
	if err := h.service.Retry(c.Param("deliveryId")); err != nil {
		notificationFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, gin.H{"queued": true})
}

func (h *NotificationsHandler) send(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var input struct {
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		notificationFailure(c, notifications.ErrInvalid)
		return
	}
	items, err := h.service.Send(input.Title, input.Message)
	if err != nil {
		notificationFailure(c, err)
		return
	}
	if len(items) == 0 {
		Failure(c, http.StatusUnprocessableEntity, "NOTIFICATION_NO_CHANNELS", "请先启用接收自定义通知的渠道", nil)
		return
	}
	Success(c, http.StatusAccepted, gin.H{"items": items})
}

func notificationFailure(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "NOTIFICATION_FAILED", "通知操作失败"
	switch {
	case errors.Is(err, notifications.ErrInvalid):
		status, code, message = http.StatusUnprocessableEntity, "NOTIFICATION_INVALID", "请检查渠道名称、事件、邮箱或 Webhook 模板"
	case errors.Is(err, notifications.ErrNotFound):
		status, code, message = http.StatusNotFound, "NOTIFICATION_NOT_FOUND", "通知渠道或记录不存在"
	case errors.Is(err, notifications.ErrConflict):
		status, code, message = http.StatusConflict, "NOTIFICATION_CONFLICT", "配置或发送状态已变化，请刷新后重试"
	case errors.Is(err, notifications.ErrLimit):
		status, code, message = http.StatusConflict, "NOTIFICATION_LIMIT", "通知渠道或待发送队列已达到上限"
	case errors.Is(err, notifications.ErrSecrets):
		status, code, message = http.StatusUnprocessableEntity, "NOTIFICATION_CREDENTIALS_UNAVAILABLE", "Webhook 凭据无法读取，请检查通知密钥文件与数据目录"
	}
	Failure(c, status, code, message, nil)
}
