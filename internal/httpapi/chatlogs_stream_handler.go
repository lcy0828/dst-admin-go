package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dont/internal/chatlogs"
	"github.com/gin-gonic/gin"
)

type chatLogStreamer interface {
	Subscribe(context.Context, string, chatlogs.Kind, string) (*chatlogs.Subscription, error)
	StreamWindow(context.Context, string, chatlogs.Kind, string) (chatlogs.StreamWindow, error)
	StreamStatus(string) chatlogs.StreamStatus
}

func (h *ChatLogHandler) stream(c *gin.Context) {
	service, ok := h.chatLogs.(chatLogStreamer)
	if !ok {
		chatLogFailure(c, chatlogs.ErrStreamUnavailable)
		return
	}
	_, provided := c.Request.URL.Query()["cursor"]
	cursor := c.Query("cursor")
	if !provided {
		cursor = strings.TrimSpace(c.GetHeader("Last-Event-ID"))
	}
	if provided && cursor == "" {
		chatLogFailure(c, chatlogs.ErrInvalidStreamCursor)
		return
	}
	roomID, kind := c.Param("roomId"), chatlogs.Kind(c.Query("kind"))
	sub, err := service.Subscribe(c.Request.Context(), roomID, kind, cursor)
	if err != nil {
		chatLogFailure(c, err)
		return
	}
	defer sub.Close()
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-store, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	controller := http.NewResponseController(c.Writer)
	defer controller.SetWriteDeadline(time.Time{})
	// Limit stale credentials and force periodic reauthorization on reconnect.
	lifetime := time.NewTimer(5 * time.Minute)
	defer lifetime.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	window := sub.Initial
	lastStatus := ""
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-sub.Done:
			return
		case <-lifetime.C:
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = writeRuntimeSSE(c.Writer, "", "chat.reconnect", gin.H{"cursor": cursor})
			_ = controller.Flush()
			return
		default:
		}
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		next, writeErr := writeChatWindow(c.Writer, window, cursor)
		if writeErr != nil {
			return
		}
		cursor = next
		status := service.StreamStatus(roomID)
		statusKey := fmt.Sprintf("%s/%t", status.State, status.Partial)
		if statusKey != lastStatus {
			if err = writeRuntimeSSE(c.Writer, "", "chat.status", status); err != nil {
				return
			}
			lastStatus = statusKey
		}
		if err = controller.Flush(); err != nil {
			return
		}
		_ = controller.SetWriteDeadline(time.Time{})
		if !window.More {
			select {
			case <-c.Request.Context().Done():
				return
			case <-sub.Done:
				return
			case <-lifetime.C:
				_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_ = writeRuntimeSSE(c.Writer, "", "chat.reconnect", gin.H{"cursor": cursor})
				_ = controller.Flush()
				return
			case <-heartbeat.C:
				lastStatus = "" // Refresh sync time even when the state is unchanged.
				_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err = fmt.Fprint(c.Writer, ": heartbeat\n\n"); err != nil {
					return
				}
				if err = controller.Flush(); err != nil {
					return
				}
				_ = controller.SetWriteDeadline(time.Time{})
			case <-sub.Wake:
			}
		}
		window, err = service.StreamWindow(c.Request.Context(), roomID, kind, cursor)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_ = writeRuntimeSSE(c.Writer, "", "chat.error", gin.H{"code": "CHAT_STREAM_READ_FAILED", "message": "聊天记录暂时无法同步，请保留游标后重连。"})
				_ = controller.Flush()
			}
			return
		}
	}
}

func writeChatWindow(writer http.ResponseWriter, window chatlogs.StreamWindow, previous string) (string, error) {
	if window.Signal != "" {
		reason := ""
		if window.Signal == "chat.gap" {
			reason = "RETENTION_EXCEEDED"
		}
		if window.Signal == "chat.reset" {
			reason = "STREAM_REPLACED_OR_RESTORED"
		}
		if err := writeRuntimeSSE(writer, window.Cursor, window.Signal, gin.H{"cursor": window.Cursor, "reason": reason}); err != nil {
			return previous, err
		}
		previous = window.Cursor
	}
	for _, item := range window.Items {
		if err := writeRuntimeSSE(writer, item.Cursor, "chat.record", item.Change); err != nil {
			return previous, err
		}
		previous = item.Cursor
	}
	// Filtered-out changes also advance the resume point. A client must persist
	// these cursor frames, without treating them as chat messages.
	if window.Cursor != previous {
		if err := writeRuntimeSSE(writer, window.Cursor, "chat.cursor", gin.H{"cursor": window.Cursor}); err != nil {
			return previous, err
		}
	}
	return window.Cursor, nil
}
