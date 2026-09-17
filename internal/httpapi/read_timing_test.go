package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dont/internal/mods"
	"dont/internal/requesttiming"
	"dont/internal/worldstate"

	"github.com/gin-gonic/gin"
)

type timedModListFixture struct {
	modPlacementReaderFixture
	err error
}

func (f timedModListFixture) RoomList(ctx context.Context, _ string) (mods.ModList, error) {
	defer requesttiming.Start(ctx, "fixture.mods")()
	return mods.ModList{Items: []mods.ModState{}}, f.err
}

type timedWorldListFixture struct {
	worldStateHandlerService
	err error
}

func (f timedWorldListFixture) List(ctx context.Context, _ string) (worldstate.List, error) {
	defer requesttiming.Start(ctx, "fixture.worlds")()
	return worldstate.List{Items: []worldstate.Snapshot{}}, f.err
}

func TestReadTimingHeadersPrecedeSuccessAndFailureBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, failure := range []error{nil, errors.New("fixture read failure"), context.Canceled} {
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		t.Cleanup(func() { log.SetOutput(previous) })
		router := gin.New()
		router.Use(RequestContext())
		v2 := router.Group("/api/v2")
		modHandler := NewModHandler(modHandlerService{}, nil)
		modHandler.ConfigurePlacementReader(timedModListFixture{err: failure})
		modHandler.Register(v2)
		NewWorldStateHandler(timedWorldListFixture{err: failure}).Register(v2)
		for _, test := range []struct{ path, metric string }{
			{path: "/api/v2/rooms/room/mods", metric: "fixture.mods"},
			{path: "/api/v2/rooms/room/world-states", metric: "fixture.worlds"},
		} {
			output.Reset()
			response := performJSON(router, http.MethodGet, test.path, nil, nil, "")
			wantStatus := http.StatusOK
			if failure != nil {
				wantStatus = http.StatusInternalServerError
			}
			assertStatus(t, response, wantStatus)
			header := response.Result().Header.Get("Server-Timing")
			if !strings.Contains(header, "handler;dur=") || !strings.Contains(header, test.metric+";dur=") {
				t.Fatalf("timing header missing from %s response: %s", test.path, header)
			}
			if strings.Contains(response.Body.String(), "duration_ms") {
				t.Fatal("timing changed the business response body")
			}
			if failure == nil && strings.Contains(output.String(), "[ReadTiming]") {
				t.Fatalf("normal reads still log full timings: %s", output.String())
			}
			if failure != nil && (!strings.Contains(output.String(), "[ReadTiming]") || !strings.Contains(output.String(), "failed=true") || !strings.Contains(output.String(), test.metric)) {
				t.Fatalf("failed read lost its diagnostic timings: %s", output.String())
			}
		}
		response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/world-states/history?worldId=master", nil, nil, "")
		if response.Result().Header.Get("Server-Timing") != "" {
			t.Fatal("unrelated routes unexpectedly enabled timing")
		}
		log.SetOutput(previous)
	}
}

func TestSlowReadRetainsDetailedLogAndResponseTiming(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	router := gin.New()
	router.Use(RequestContext())
	router.GET("/api/v2/timed-read", func(c *gin.Context) {
		ctx, recorder := requesttiming.New(c.Request.Context())
		finish := requesttiming.Start(ctx, "fixture.slow")
		time.Sleep(550 * time.Millisecond)
		finish()
		finishReadTiming(c, recorder, nil)
		c.Status(http.StatusOK)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/timed-read", nil))
	if !strings.Contains(output.String(), "[ReadTiming]") || !strings.Contains(output.String(), "fixture.slow") || !strings.Contains(output.String(), "failed=false") {
		t.Fatalf("slow successful read lost its diagnostic timings: %s", output.String())
	}
	if !strings.Contains(response.Result().Header.Get("Server-Timing"), "fixture.slow") {
		t.Fatal("slow response lost Server-Timing")
	}
}
