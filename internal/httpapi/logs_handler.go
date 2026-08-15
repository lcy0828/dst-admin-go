package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"dont/internal/logstream"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type LogHandler struct {
	logs *logstream.Service
}

func NewLogHandler(service *logstream.Service) *LogHandler { return &LogHandler{logs: service} }

func (h *LogHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/rooms/:roomId/worlds/:worldId/logs")
	group.GET("", h.snapshot)
	group.GET("/download", h.download)
	group.GET("/events", h.events)
}

func (h *LogHandler) snapshot(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "500"))
	result, err := h.logs.Snapshot(c.Param("roomId"), c.Param("worldId"), limit, c.Query("query"))
	if err != nil {
		logFailure(c, err)
		return
	}
	Success(c, http.StatusOK, result)
}

func (h *LogHandler) download(c *gin.Context) {
	wroteHeader := false
	err := h.logs.Download(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), func(info logstream.DownloadInfo, data []byte) error {
		if !wroteHeader {
			c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, info.FileName))
			c.Header("Content-Type", "text/plain; charset=utf-8")
			c.Header("Content-Length", strconv.FormatInt(info.Size, 10))
			c.Header("Last-Modified", info.UpdatedAt.UTC().Format(http.TimeFormat))
			c.Header("X-Content-Type-Options", "nosniff")
			c.Status(http.StatusOK)
			wroteHeader = true
		}
		_, writeErr := c.Writer.Write(data)
		return writeErr
	})
	if err != nil && !wroteHeader {
		logFailure(c, err)
	}
}

func (h *LogHandler) events(c *gin.Context) {
	tail, _ := strconv.Atoi(c.DefaultQuery("tail", "200"))
	if _, err := h.logs.Snapshot(c.Param("roomId"), c.Param("worldId"), 1, ""); err != nil {
		logFailure(c, err)
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		Failure(c, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "当前响应器不支持日志流", nil)
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	type followResult struct{ err error }
	done := make(chan followResult, 1)
	events := make(chan logstream.Event, 64)
	go func() {
		done <- followResult{err: h.logs.Follow(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), tail, func(event logstream.Event) error {
			select {
			case events <- event:
				return nil
			case <-c.Request.Context().Done():
				return c.Request.Context().Err()
			}
		})}
	}()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case result := <-done:
			if result.err != nil && !errors.Is(result.err, c.Request.Context().Err()) {
				c.SSEvent("error", gin.H{"code": "LOG_STREAM_FAILED", "message": result.err.Error()})
				flusher.Flush()
			}
			return
		case event := <-events:
			c.SSEvent(event.Type, event)
			flusher.Flush()
		case <-heartbeat.C:
			c.SSEvent("heartbeat", gin.H{"at": time.Now().UTC()})
			flusher.Flush()
		}
	}
}

func logFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, logstream.ErrLogNotFound):
		Failure(c, http.StatusNotFound, "LOG_NOT_FOUND", "该分片还没有生成服务器日志", nil)
	case errors.Is(err, logstream.ErrUnsafeLog), errors.Is(err, rooms.ErrUnsafePath), errors.Is(err, rooms.ErrInvalidID):
		Failure(c, http.StatusBadRequest, "UNSAFE_LOG_PATH", "日志资源路径无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "LOG_READ_FAILED", "读取服务器日志失败", nil)
	}
}
