package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
)

type JobHandler struct {
	service *jobs.Service
}

func NewJobHandler(service *jobs.Service) *JobHandler { return &JobHandler{service: service} }

func (h *JobHandler) Register(v2 *gin.RouterGroup) {
	group := v2.Group("/jobs")
	group.GET("/events", h.events)
	group.GET("", h.list)
	group.GET("/:jobId", h.get)
	group.POST("/:jobId/cancel", h.cancel)
}

func (h *JobHandler) list(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	items, total, err := h.service.List(jobs.ListFilter{
		Status: jobs.Status(c.Query("status")), Kind: c.Query("kind"), Limit: limit, Offset: offset,
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_LIST_FAILED", "无法读取任务列表", nil)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": total, "limit": limit, "offset": offset})
}

func (h *JobHandler) get(c *gin.Context) {
	job, err := h.service.Get(c.Param("jobId"))
	if err != nil {
		jobFailure(c, err)
		return
	}
	Success(c, http.StatusOK, job)
}

func (h *JobHandler) cancel(c *gin.Context) {
	job, err := h.service.Cancel(c.Param("jobId"))
	if err != nil {
		jobFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func (h *JobHandler) events(c *gin.Context) {
	afterValue, hasAfter := c.GetQuery("after")
	headerValue := strings.TrimSpace(c.GetHeader("Last-Event-ID"))
	afterID := parseEventID(afterValue)
	if headerID := parseEventID(headerValue); headerID > afterID {
		afterID = headerID
	}
	hasCursor := hasAfter || headerValue != ""
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		Failure(c, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "当前响应不支持实时事件", nil)
		return
	}
	signal, unsubscribe := h.service.Subscribe()
	defer unsubscribe()
	window, err := h.service.EventWindow()
	if err != nil {
		return
	}
	reset := false
	if !hasCursor {
		afterID = window.LastID
	} else if afterID > window.LastID || window.FirstID > 0 && afterID < window.FirstID-1 {
		afterID = window.LastID
		reset = true
	}
	if _, err := fmt.Fprintf(c.Writer, "id: %d\nevent: job.cursor\ndata: {\"watermark\":%d,\"reset\":%t}\n\n", afterID, afterID, reset); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		events, err := h.service.EventsAfter(afterID, 100)
		if err != nil {
			return
		}
		for _, event := range events {
			encoded, err := json.Marshal(event)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(c.Writer, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, encoded); err != nil {
				return
			}
			afterID = event.ID
		}
		if len(events) > 0 {
			flusher.Flush()
			if len(events) == 100 {
				continue
			}
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-signal:
			if _, err := fmt.Fprint(c.Writer, "event: refresh\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(c.Writer, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseEventID(value string) int64 {
	value = strings.TrimSpace(value)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func jobFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		Failure(c, http.StatusNotFound, "JOB_NOT_FOUND", "任务不存在", nil)
	case errors.Is(err, jobs.ErrNotCancelable):
		Failure(c, http.StatusConflict, "JOB_NOT_CANCELABLE", "任务已经结束，无法取消", nil)
	default:
		Failure(c, http.StatusInternalServerError, "JOB_OPERATION_FAILED", "任务操作失败", nil)
	}
}
