package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dont/internal/runtimeevents"
	"dont/internal/runtimeoverview"

	"github.com/gin-gonic/gin"
)

type RuntimeObservabilityHandler struct {
	events   *runtimeevents.Service
	overview *runtimeoverview.Service
}

func NewRuntimeObservabilityHandler(events *runtimeevents.Service, overview *runtimeoverview.Service) (*RuntimeObservabilityHandler, error) {
	if events == nil || overview == nil {
		return nil, errors.New("runtime observability services are required")
	}
	return &RuntimeObservabilityHandler{events: events, overview: overview}, nil
}

func (h *RuntimeObservabilityHandler) Register(v2 *gin.RouterGroup) {
	room := v2.Group("/rooms/:roomId")
	room.GET("/runtime/overview", h.roomOverview)
	room.GET("/worlds/:worldId/runtime/events/stream", h.eventStream)
}

func (h *RuntimeObservabilityHandler) roomOverview(c *gin.Context) {
	value, err := h.overview.Snapshot(c.Request.Context(), c.Param("roomId"))
	if err != nil {
		topologyFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *RuntimeObservabilityHandler) eventStream(c *gin.Context) {
	cursorValue, resume := c.GetQuery("cursor")
	if !resume {
		cursorValue = strings.TrimSpace(c.GetHeader("Last-Event-ID"))
		resume = cursorValue != ""
	}
	var cursor runtimeevents.Cursor
	var err error
	if resume {
		cursor, err = runtimeevents.Parse(cursorValue)
		if err != nil {
			Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_EVENT_CURSOR", "Runtime 事件续读游标无效", nil)
			return
		}
	}
	window, err := h.events.Window(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), cursor, resume)
	if err != nil {
		dstRuntimeFailure(c, err)
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		Failure(c, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "当前响应器不支持 Runtime 事件流", nil)
		return
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	cursor, err = writeRuntimeWindow(c.Writer, window, true)
	if err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	poll := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	defer poll.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(c.Writer, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			next, readErr := h.events.Window(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), cursor, true)
			if readErr != nil {
				_ = writeRuntimeSSE(c.Writer, "", "runtime.error", gin.H{"code": "RUNTIME_EVENT_STREAM_FAILED", "message": readErr.Error()})
				flusher.Flush()
				return
			}
			if len(next.Events) == 0 && !next.Reset && !next.Gap {
				continue
			}
			cursor, err = writeRuntimeWindow(c.Writer, next, false)
			if err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeRuntimeWindow(writer http.ResponseWriter, window runtimeevents.Window, initial bool) (runtimeevents.Cursor, error) {
	if window.Reset {
		if err := writeRuntimeSSE(writer, "", "runtime.reset", gin.H{
			"cursor": window.EncodedCursor, "firstSequence": window.FirstSequence, "lastSequence": window.LastSequence,
		}); err != nil {
			return runtimeevents.Cursor{}, err
		}
	}
	if window.Gap {
		if err := writeRuntimeSSE(writer, "", "runtime.gap", gin.H{
			"cursor": window.EncodedCursor, "firstSequence": window.FirstSequence, "lastSequence": window.LastSequence,
		}); err != nil {
			return runtimeevents.Cursor{}, err
		}
	}
	cursor := window.Cursor
	if len(window.Events) == 0 {
		if initial || window.Reset || window.Gap {
			if err := writeRuntimeSSE(writer, window.EncodedCursor, "runtime.cursor", gin.H{
				"cursor": window.EncodedCursor, "firstSequence": window.FirstSequence, "lastSequence": window.LastSequence,
			}); err != nil {
				return runtimeevents.Cursor{}, err
			}
		}
		return cursor, nil
	}
	for _, event := range window.Events {
		encodedCursor, err := runtimeevents.EventCursor(window.Cursor.ProducerInstanceID, event.Sequence)
		if err != nil {
			return runtimeevents.Cursor{}, err
		}
		if err := writeRuntimeSSE(writer, encodedCursor, "runtime.event", gin.H{
			"cursor": encodedCursor, "producerInstanceId": window.Cursor.ProducerInstanceID, "event": event,
		}); err != nil {
			return runtimeevents.Cursor{}, err
		}
		cursor.Sequence = event.Sequence
	}
	return cursor, nil
}

func writeRuntimeSSE(writer http.ResponseWriter, id, event string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(writer, "id: %s\n", id); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, data)
	return err
}
