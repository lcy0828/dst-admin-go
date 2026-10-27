package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"dont/internal/jobs"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AIBackend adapts a fixed tool catalog to the same handlers used by the UI.
// It does not proxy arbitrary URLs, create loopback sessions or bypass the
// existing operation planning, capacity checks, leases and command receipts.
type AIBackend struct {
	rooms     *RoomHandler
	logs      *LogHandler
	overview  *RuntimeObservabilityHandler
	runtime   *DSTRuntimeHandler
	states    *WorldStateHandler
	audit     *RuntimeAuditHandler
	jobs      *JobHandler
	console   *ConsoleHandler
	member    bool
	queries   AIPlatformQueries
	apiRouter http.Handler
	apiFiles  *aicontrol.Store
}

func NewAIBackend(rooms *RoomHandler, logs *LogHandler, overview *RuntimeObservabilityHandler, runtime *DSTRuntimeHandler, states *WorldStateHandler, audit *RuntimeAuditHandler, jobs *JobHandler, console *ConsoleHandler, member bool, queries ...AIPlatformQueries) *AIBackend {
	b := &AIBackend{rooms: rooms, logs: logs, overview: overview, runtime: runtime, states: states, audit: audit, jobs: jobs, console: console, member: member}
	if len(queries) > 0 {
		b.queries = queries[0]
	}
	return b
}
func (b *AIBackend) Invoke(ctx context.Context, name string, a aicontrol.Arguments, selectedRoom string) (json.RawMessage, error) {
	visible := func(id string) bool { return selectedRoom == "" || selectedRoom == id }
	if a.RoomID != "" && !visible(a.RoomID) {
		return nil, aicontrol.ErrDenied
	}
	// Compatibility tool names keep their compact results, but production
	// mutations now traverse the very same authenticated router as generated tools.
	if b.apiRouter != nil && (name == "room_control" || name == "world_console") {
		catalog, err := apicontract.Default()
		if err != nil {
			return nil, err
		}
		id := "runRoomAction"
		body := map[string]any{}
		if a.RuntimeMode != "" {
			body["runtimeMode"] = a.RuntimeMode
		}
		input := apicontract.Input{Path: map[string]any{"roomId": a.RoomID, "action": a.Action}, Body: body, RequestID: a.RequestID}
		if name == "world_console" {
			id = "executeRawRoomCommand"
			room, err := b.rooms.rooms.Room(a.RoomID)
			if err != nil {
				return nil, err
			}
			input.Path = map[string]any{"roomId": a.RoomID, "worldId": a.WorldID}
			input.Body = map[string]any{"command": a.Command, "confirmation": room.Name}
		}
		op, ok := catalog.Operation(id)
		if !ok {
			return nil, aicontrol.ErrUnavailable
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		input, err = op.Validate(raw)
		if err != nil {
			return nil, err
		}
		value, err := b.InvokeAPI(ctx, aicontrol.Principal{}, op, input)
		if err != nil {
			return nil, err
		}
		var result struct {
			Data json.RawMessage `json:"data"`
		}
		if err = json.Unmarshal(value, &result); err != nil {
			return nil, err
		}
		return result.Data, nil
	}
	switch name {
	case "room_performance":
		return b.roomPerformance(ctx, a.RoomID)
	case "room_players":
		return b.roomPlayers(ctx, a)
	case "room_mods":
		return b.roomMods(ctx, a)
	case "room_backups":
		return b.roomBackups(a.RoomID)
	case "room_automation":
		return b.roomAutomation(a)
	}
	if name == "list_rooms" {
		items, err := b.rooms.rooms.List()
		if err != nil {
			return nil, err
		}
		rows := []any{}
		for _, room := range items {
			if visible(room.ID) {
				rows = append(rows, map[string]any{"id": room.ID, "name": room.Name, "worldCount": room.WorldCount, "controlState": room.ControlState})
			}
		}
		return json.Marshal(map[string]any{"items": rows})
	}
	if name == "list_jobs" {
		items, total, err := b.jobs.service.List(jobs.ListFilter{RoomID: a.RoomID, Limit: 25})
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"items": items, "total": total, "limit": 25})
	}
	if name == "get_job" {
		job, err := b.jobs.service.Get(a.JobID)
		if err != nil {
			return nil, err
		}
		if !visible(job.RoomID) || job.RoomID != a.RoomID {
			return nil, aicontrol.ErrDenied
		}
		return json.Marshal(job)
	}
	method := http.MethodGet
	query := url.Values{}
	body := map[string]any{}
	var handler gin.HandlerFunc
	switch name {
	case "room_status":
		handler = func(c *gin.Context) {
			value, err := b.overview.overview.Snapshot(c.Request.Context(), a.RoomID)
			if err != nil {
				topologyFailure(c, err)
				return
			}
			if b.refreshAIObservation(c.Request.Context(), value, a.IncludeStartupPlan) {
				value, err = b.overview.overview.Snapshot(c.Request.Context(), a.RoomID)
				if err != nil {
					topologyFailure(c, err)
					return
				}
			}
			Success(c, http.StatusOK, aiRoomStatus(value, a.IncludeStartupPlan))
		}
	case "runtime_modes":
		handler = b.rooms.runtimeModes
	case "world_logs":
		handler = b.logs.snapshot
		limit := a.Limit
		if limit == 0 {
			limit = 100
		}
		query.Set("limit", strconv.Itoa(limit))
		query.Set("query", a.Query)
	case "room_events":
		handler = b.audit.list
		query.Set("limit", "25")
		query.Set("worldId", a.WorldID)
	case "latest_diagnostic":
		handler = b.runtime.latestDiagnostic
	case "world_state":
		handler = func(c *gin.Context) {
			value, err := b.states.states.List(c.Request.Context(), a.RoomID)
			if err != nil {
				worldStateFailure(c, err)
				return
			}
			for _, item := range value.Items {
				if item.WorldID == a.WorldID {
					Success(c, http.StatusOK, item)
					return
				}
			}
			NotFound(c)
		}
	case "command_receipt":
		if a.Refresh {
			query.Set("refresh", "true")
		}
		handler = b.console.run
	case "room_control":
		if b.member {
			return nil, aicontrol.ErrDenied
		}
		handler = b.rooms.action
		method = http.MethodPost
		body["runtimeMode"] = a.RuntimeMode
	case "world_console":
		if b.member {
			return nil, aicontrol.ErrDenied
		}
		handler = b.console.executeRaw
		method = http.MethodPost
		room, err := b.rooms.rooms.Room(a.RoomID)
		if err != nil {
			return nil, err
		}
		body["command"] = a.Command
		body["confirmation"] = room.Name
	default:
		return nil, aicontrol.ErrDenied
	}
	encoded, _ := json.Marshal(body)
	request, _ := http.NewRequestWithContext(ctx, method, "http://dst-admin.internal/ai-tool?"+query.Encode(), bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	writer := &aiResponseWriter{header: http.Header{}, status: 200}
	c := &gin.Context{Request: request, Writer: writer, Params: gin.Params{{Key: "roomId", Value: a.RoomID}, {Key: "worldId", Value: a.WorldID}, {Key: "action", Value: a.Action}, {Key: "runId", Value: a.RunID}}}
	c.Set(requestIDKey, "ai-"+uuid.NewString())
	c.Set("dst_admin.ai_action", true)
	handler(c)
	if writer.overflow {
		return nil, fmt.Errorf("AI_RESULT_TOO_LARGE")
	}
	var response struct {
		Data  json.RawMessage `json:"data"`
		Error *errorBody      `json:"error"`
	}
	if json.Unmarshal(writer.body.Bytes(), &response) != nil {
		return nil, fmt.Errorf("AI_INVALID_PLATFORM_RESPONSE")
	}
	if response.Error != nil {
		detail, _ := json.Marshal(response.Error)
		return nil, fmt.Errorf("%s", detail)
	}
	return response.Data, nil
}

type aiResponseWriter struct {
	header            http.Header
	body              bytes.Buffer
	status            int
	written, overflow bool
}

func (w *aiResponseWriter) Header() http.Header { return w.header }
func (w *aiResponseWriter) WriteHeader(n int)   { w.status = n; w.written = true }
func (w *aiResponseWriter) Write(p []byte) (int, error) {
	w.written = true
	if w.body.Len()+len(p) > 256<<10 {
		w.overflow = true
		return len(p), nil
	}
	return w.body.Write(p)
}
func (w *aiResponseWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *aiResponseWriter) Status() int                       { return w.status }
func (w *aiResponseWriter) Size() int                         { return w.body.Len() }
func (w *aiResponseWriter) Written() bool                     { return w.written }
func (w *aiResponseWriter) WriteHeaderNow()                   { w.written = true }
func (w *aiResponseWriter) Flush()                            {}
func (w *aiResponseWriter) CloseNotify() <-chan bool          { return make(chan bool) }
func (w *aiResponseWriter) Pusher() http.Pusher               { return nil }
func (w *aiResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, fmt.Errorf("AI_STREAM_UNSUPPORTED")
}

var _ gin.ResponseWriter = (*aiResponseWriter)(nil)
var _ io.Writer = (*aiResponseWriter)(nil)
