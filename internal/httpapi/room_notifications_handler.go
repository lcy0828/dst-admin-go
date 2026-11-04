package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"dont/internal/notifications"
	"github.com/gin-gonic/gin"
)

func (h *NotificationsHandler) checkRoom(c *gin.Context) bool {
	if h.roomExists == nil || h.roomExists(c.Param("roomId")) != nil {
		NotFound(c)
		return false
	}
	return true
}

func (h *NotificationsHandler) roomEvents(c *gin.Context) {
	if !h.checkRoom(c) {
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	tail, tailErr := strconv.ParseBool(c.DefaultQuery("tail", "false"))
	if err != nil || tailErr != nil {
		notificationFailure(c, notifications.ErrInvalid)
		return
	}
	value, err := h.service.RoomFeed(c.Param("roomId"), c.Query("after"), c.Query("kinds"), limit, tail)
	if err != nil {
		notificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *NotificationsHandler) recentRoomEvents(c *gin.Context) {
	if !h.checkRoom(c) {
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if err != nil {
		notificationFailure(c, notifications.ErrInvalid)
		return
	}
	items, err := h.service.RecentRoom(c.Param("roomId"), limit)
	if err != nil {
		notificationFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, gin.H{"items": items, "retentionDays": 30, "complete": false})
}

func (h *NotificationsHandler) streamRoomEvents(c *gin.Context) {
	select {
	case h.streams <- struct{}{}:
		defer func() { <-h.streams }()
	default:
		c.Header("Retry-After", "15")
		Failure(c, http.StatusTooManyRequests, "NOTIFICATION_STREAM_LIMIT", "连接较多，请稍后重试", nil)
		return
	}
	if !h.checkRoom(c) {
		return
	}
	cursor := c.Query("after")
	if cursor == "" {
		cursor = strings.TrimSpace(c.GetHeader("Last-Event-ID"))
	}
	wake, closeSub := h.service.Subscribe()
	defer closeSub()
	read := func() (notifications.Feed, error) {
		return h.service.RoomFeed(c.Param("roomId"), cursor, c.Query("kinds"), 50, cursor == "")
	}
	page, err := read()
	if err != nil {
		notificationFailure(c, err)
		return
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-store, no-transform")
	c.Header("X-Accel-Buffering", "no")
	controller := http.NewResponseController(c.Writer)
	defer controller.SetWriteDeadline(time.Time{})
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	lifetime := time.NewTimer(time.Minute)
	defer lifetime.Stop()
	for {
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err = writeRuntimeSSE(c.Writer, page.NextCursor, "notice.page", page); err != nil {
			return
		}
		if controller.Flush() != nil {
			return
		}
		_ = controller.SetWriteDeadline(time.Time{})
		cursor = page.NextCursor
		if !page.HasMore {
		waitForEvent:
			for {
				select {
				case <-c.Request.Context().Done():
					return
				case <-lifetime.C:
					return // Reauthorize on reconnect, including revoked room scopes.
				case <-heartbeat.C:
					_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
					if _, err = c.Writer.Write([]byte(": heartbeat\n\n")); err != nil || controller.Flush() != nil {
						return
					}
					_ = controller.SetWriteDeadline(time.Time{})
					continue
				case <-wake:
					break waitForEvent
				}
			}
		}
		page, err = read()
		if err != nil {
			return
		}
	}
}
