package httpapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dont/internal/dstruntime"
	"dont/internal/runtimeevents"

	"github.com/gin-gonic/gin"
)

type runtimeEventHTTPSource struct {
	batch dstruntime.EventBatch
	calls int
}

func (s *runtimeEventHTTPSource) ReadEvents(context.Context, string, string) (dstruntime.EventBatch, error) {
	s.calls++
	return s.batch, nil
}

func TestRuntimeEventStreamRejectsInvalidCursor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	source := &runtimeEventHTTPSource{}
	service, err := runtimeevents.New(source)
	if err != nil {
		t.Fatal(err)
	}
	handler := &RuntimeObservabilityHandler{events: service}
	router := gin.New()
	router.GET("/api/v2/rooms/:roomId/worlds/:worldId/runtime/events/stream", handler.eventStream)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v2/rooms/room/worlds/master/runtime/events/stream?cursor=invalid", nil)
	router.ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_RUNTIME_EVENT_CURSOR")
	if source.calls != 0 {
		t.Fatalf("event source calls = %d, expected malformed cursor to fail before reading", source.calls)
	}
}

func TestRuntimeEventStreamResumesFromLastEventID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	batch := dstruntime.EventBatch{
		ProducerInstanceID: "runtime-instance", FirstSequence: 4, LastSequence: 6,
		Events: []dstruntime.RuntimeEvent{
			{Sequence: 4, Kind: "player.joined"},
			{Sequence: 5, Kind: "world.phase"},
			{Sequence: 6, Kind: "player.left"},
		},
	}
	source := &runtimeEventHTTPSource{batch: batch}
	service, err := runtimeevents.New(source)
	if err != nil {
		t.Fatal(err)
	}
	handler := &RuntimeObservabilityHandler{events: service}
	router := gin.New()
	router.GET("/api/v2/rooms/:roomId/worlds/:worldId/runtime/events/stream", handler.eventStream)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/rooms/room/worlds/master/runtime/events/stream", nil)
	request.Header.Set("Last-Event-ID", runtimeevents.Encode(runtimeevents.Cursor{ProducerInstanceID: batch.ProducerInstanceID, Sequence: 4}))
	requestContext, cancel := context.WithCancel(request.Context())
	cancel()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request.WithContext(requestContext))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	want := []string{
		runtimeevents.Encode(runtimeevents.Cursor{ProducerInstanceID: batch.ProducerInstanceID, Sequence: 5}),
		runtimeevents.Encode(runtimeevents.Cursor{ProducerInstanceID: batch.ProducerInstanceID, Sequence: 6}),
	}
	if got := runtimeStreamEventIDs(t, response.Body.String()); !equalStrings(got, want) {
		t.Fatalf("resumed event ids = %v, expected %v; body=%s", got, want, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "event: runtime.cursor") || strings.Count(response.Body.String(), "event: runtime.event") != 2 {
		t.Fatalf("resumed stream emitted unexpected frames: %s", response.Body.String())
	}
}

func TestRuntimeResetAndGapSignalsDoNotAdvanceEventID(t *testing.T) {
	producer := "runtime-instance"
	for _, signal := range []struct {
		name  string
		reset bool
		gap   bool
		event string
	}{
		{name: "reset", reset: true, event: "runtime.reset"},
		{name: "gap", gap: true, event: "runtime.gap"},
	} {
		t.Run(signal.name, func(t *testing.T) {
			window := runtimeevents.Window{
				Cursor:        runtimeevents.Cursor{ProducerInstanceID: producer, Sequence: 6},
				EncodedCursor: runtimeevents.Encode(runtimeevents.Cursor{ProducerInstanceID: producer, Sequence: 6}),
				FirstSequence: 4, LastSequence: 6, Reset: signal.reset, Gap: signal.gap,
				Events: []dstruntime.RuntimeEvent{{Sequence: 4}, {Sequence: 5}, {Sequence: 6}},
			}
			response := httptest.NewRecorder()
			if _, err := writeRuntimeWindow(response, window, true); err != nil {
				t.Fatal(err)
			}
			frames := strings.Split(strings.TrimSpace(response.Body.String()), "\n\n")
			if len(frames) != 4 || !strings.Contains(frames[0], "event: "+signal.event) || strings.Contains(frames[0], "id: ") {
				t.Fatalf("signal frame advanced Last-Event-ID: %q", frames[0])
			}
			want := []string{
				runtimeevents.Encode(runtimeevents.Cursor{ProducerInstanceID: producer, Sequence: 4}),
				runtimeevents.Encode(runtimeevents.Cursor{ProducerInstanceID: producer, Sequence: 5}),
				runtimeevents.Encode(runtimeevents.Cursor{ProducerInstanceID: producer, Sequence: 6}),
			}
			if got := runtimeStreamEventIDs(t, response.Body.String()); !equalStrings(got, want) {
				t.Fatalf("event ids = %v, expected %v", got, want)
			}
		})
	}
}

func runtimeStreamEventIDs(t *testing.T, body string) []string {
	t.Helper()
	ids := make([]string, 0)
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		if line := scanner.Text(); strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimPrefix(line, "id: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
