package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dont/internal/fleetoverview"
	"dont/internal/runtimeevents"
	"dont/internal/runtimeobservation"
	"dont/internal/runtimeoverview"

	"github.com/gin-gonic/gin"
)

type RuntimeObservabilityHandler struct {
	events       *runtimeevents.Service
	overview     *runtimeoverview.Service
	fleet        *fleetoverview.Service
	observations *runtimeobservation.Coordinator
}

func NewRuntimeObservabilityHandler(events *runtimeevents.Service, overview *runtimeoverview.Service, fleet *fleetoverview.Service, observations ...*runtimeobservation.Coordinator) (*RuntimeObservabilityHandler, error) {
	if events == nil || overview == nil || fleet == nil {
		return nil, errors.New("runtime observability services are required")
	}
	handler := &RuntimeObservabilityHandler{events: events, overview: overview, fleet: fleet}
	if len(observations) > 0 {
		handler.observations = observations[0]
	}
	return handler, nil
}

func (h *RuntimeObservabilityHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/runtime-overview", h.fleetOverview)
	if h.observations != nil {
		v2.GET("/runtime-observations/stream", h.observationStream)
		v2.POST("/runtime-observations/actions/refresh", h.refreshObservations)
	}
	room := v2.Group("/rooms/:roomId")
	room.GET("/runtime/overview", h.roomOverview)
	room.GET("/worlds/:worldId/runtime/events/stream", h.eventStream)
}

type refreshRuntimeObservationsRequest struct {
	TargetID string `json:"targetId"`
	RoomID   string `json:"roomId"`
}

func (h *RuntimeObservabilityHandler) refreshObservations(c *gin.Context) {
	var request refreshRuntimeObservationsRequest
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_OBSERVATION_REQUEST", "运行状态刷新参数无效", nil)
		return
	}
	items, err := h.observations.Refresh(c.Request.Context(), runtimeobservation.Scope{
		TargetID: strings.TrimSpace(request.TargetID), RoomID: strings.TrimSpace(request.RoomID),
	})
	if err != nil {
		runtimeObservationFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items})
}

func (h *RuntimeObservabilityHandler) observationStream(c *gin.Context) {
	scope := runtimeobservation.Scope{
		TargetID: strings.TrimSpace(c.Query("targetId")), RoomID: strings.TrimSpace(c.Query("roomId")),
	}
	updates, unsubscribe := h.observations.Subscribe()
	defer unsubscribe()
	release, err := h.observations.Acquire(c.Request.Context(), scope)
	if err != nil {
		runtimeObservationFailure(c, err)
		return
	}
	defer release()
	snapshot, err := h.observations.Snapshot(c.Request.Context(), scope)
	if err != nil {
		runtimeObservationFailure(c, err)
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		Failure(c, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "当前响应器不支持运行状态事件流", nil)
		return
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	if err := writeRuntimeSSE(c.Writer, "", "observation.snapshot", gin.H{"scope": scope, "items": snapshot}); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(c.Writer, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event := <-updates:
			if scope.TargetID != "" && event.Observation.TargetID != scope.TargetID {
				continue
			}
			if err := writeRuntimeSSE(c.Writer, fmt.Sprint(event.Sequence), event.Type, event); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func runtimeObservationFailure(c *gin.Context, err error) {
	if errors.Is(err, runtimeobservation.ErrRuntimeTargetNotFound) {
		Failure(c, http.StatusNotFound, "RUNTIME_TARGET_NOT_FOUND", "运行目标不存在", nil)
		return
	}
	Failure(c, http.StatusServiceUnavailable, "RUNTIME_OBSERVATION_FAILED", "无法刷新运行状态: "+err.Error(), nil)
}

func (h *RuntimeObservabilityHandler) fleetOverview(c *gin.Context) {
	targetID := strings.TrimSpace(c.Query("targetId"))
	readOverview := h.fleet.Snapshot
	switch strings.TrimSpace(c.Query("detail")) {
	case "", "full":
	case "inventory":
		readOverview = h.fleet.InventorySnapshot
	default:
		Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_OVERVIEW_DETAIL", "运行概览详细程度无效", nil)
		return
	}
	if h.observations != nil {
		if _, err := h.observations.Refresh(c.Request.Context(), runtimeobservation.Scope{TargetID: targetID}); err != nil {
			runtimeObservationFailure(c, err)
			return
		}
	}
	value, err := readOverview(c.Request.Context(), targetID)
	if err != nil {
		if errors.Is(err, fleetoverview.ErrRuntimeTargetNotFound) {
			Failure(c, http.StatusNotFound, "RUNTIME_TARGET_NOT_FOUND", "运行目标不存在", nil)
			return
		}
		Failure(c, http.StatusInternalServerError, "RUNTIME_OVERVIEW_FAILED", "无法生成运行概览", nil)
		return
	}
	Success(c, http.StatusOK, value)
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
	poll := time.NewTimer(runtimeEventPollDelay(0))
	defer heartbeat.Stop()
	defer poll.Stop()
	emptyPolls := 0
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
				emptyPolls++
				poll.Reset(runtimeEventPollDelay(emptyPolls))
				continue
			}
			emptyPolls = 0
			cursor, err = writeRuntimeWindow(c.Writer, next, false)
			if err != nil {
				return
			}
			flusher.Flush()
			poll.Reset(runtimeEventPollDelay(emptyPolls))
		}
	}
}

func runtimeEventPollDelay(emptyPolls int) time.Duration {
	switch {
	case emptyPolls < 5:
		return time.Second
	case emptyPolls < 12:
		return 5 * time.Second
	default:
		return 15 * time.Second
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
