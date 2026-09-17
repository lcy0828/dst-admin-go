package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/fleetoverview"
	"dont/internal/rooms"
	"dont/internal/runtimeobservation"
	"dont/internal/topology"
	"dont/shared"

	"github.com/gin-gonic/gin"
)

type runtimeObservationHTTPSource struct {
	mu                 sync.Mutex
	items              []agents.RuntimeTargetInventory
	collect            func(context.Context, agents.RuntimeTargetInventory) (agents.RuntimeTargetInventory, error)
	collects           int
	collectedTargetIDs []string
}

func (s *runtimeObservationHTTPSource) CachedRuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agents.RuntimeTargetInventory(nil), s.items...), nil
}

func (s *runtimeObservationHTTPSource) CollectRuntimeTargetInventory(ctx context.Context, targetID, installationID string) (agents.RuntimeTargetInventory, error) {
	s.mu.Lock()
	s.collects++
	s.collectedTargetIDs = append(s.collectedTargetIDs, targetID)
	var selected agents.RuntimeTargetInventory
	for _, item := range s.items {
		if item.Target.ID == targetID && item.Target.Config.InstallationID == installationID {
			selected = item
			break
		}
	}
	collect := s.collect
	s.mu.Unlock()
	if selected.Target.ID == "" {
		return agents.RuntimeTargetInventory{}, runtimeobservation.ErrRuntimeTargetNotFound
	}
	if collect != nil {
		return collect(ctx, selected)
	}
	return selected, nil
}

func (*runtimeObservationHTTPSource) CheckpointRuntimeTargetInventory(string, shared.RuntimeInventoryReport) error {
	return nil
}

func runtimeObservationHTTPInventory(now time.Time) agents.RuntimeTargetInventory {
	return agents.RuntimeTargetInventory{
		Target: agents.RuntimeTarget{
			ID: "agent:node", AgentID: "node", Name: "node", Online: true, Configured: true,
			Config: agents.RuntimeConfig{InstallationID: "default"},
		},
		Available: true,
		Inventory: shared.RuntimeInventoryReport{
			ProtocolVersion: 1, ObservedAt: now,
			Installation: shared.RuntimeInstallationReport{ID: "default"},
			Rooms:        []shared.RoomInventoryReport{}, Processes: []shared.ShardProcessReport{}, Warnings: []string{},
		},
		ObservedAt: &now, ReceivedAt: &now,
	}
}

func newRuntimeObservationHTTPRouter(t *testing.T) (*gin.Engine, *runtimeObservationHTTPSource, *runtimeobservation.Coordinator) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	source := &runtimeObservationHTTPSource{items: []agents.RuntimeTargetInventory{runtimeObservationHTTPInventory(now)}}
	coordinator, err := runtimeobservation.New(source, runtimeobservation.Options{
		Now: func() time.Time { return now }, RefreshInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	handler := &RuntimeObservabilityHandler{observations: coordinator}
	router := gin.New()
	router.GET("/api/v2/runtime-observations/stream", handler.observationStream)
	router.POST("/api/v2/runtime-observations/actions/refresh", handler.refreshObservations)
	return router, source, coordinator
}

type runtimeOverviewHTTPTopology struct {
	observations *runtimeobservation.Coordinator
}

func (s runtimeOverviewHTTPTopology) FleetTopology(ctx context.Context) (topology.FleetSnapshot, error) {
	items, err := s.observations.RuntimeTargetInventories(ctx)
	if err != nil {
		return topology.FleetSnapshot{}, err
	}
	targets := make([]topology.TargetSummary, 0, len(items))
	for _, item := range items {
		targets = append(targets, topology.TargetSummary{
			ID: item.Target.ID, Name: item.Target.Name, Kind: item.Target.Kind,
			Status: item.Target.Status, Online: item.Target.Online, Configured: item.Target.Configured,
			InventoryAvailable: item.Available, InventoryStale: item.Stale, StaleReason: item.StaleReason,
			ObservationState: item.ObservationState, ObservationError: item.ObservationError,
			RefreshStartedAt: item.RefreshStartedAt, ObservedAt: item.ObservedAt,
		})
	}
	return topology.FleetSnapshot{Targets: targets, Rooms: []topology.Snapshot{}, ObservedAt: time.Now().UTC()}, nil
}

type runtimeOverviewHTTPRooms struct{}

func (runtimeOverviewHTTPRooms) List() ([]rooms.Room, error)          { return []rooms.Room{}, nil }
func (runtimeOverviewHTTPRooms) Worlds(string) ([]rooms.World, error) { return []rooms.World{}, nil }

type runtimeOverviewHTTPRuntime struct{}

func (runtimeOverviewHTTPRuntime) Status(context.Context, string, string) (shared.ShardRuntimeStatus, error) {
	return shared.ShardRuntimeStatus{State: "stopped"}, nil
}

func newFleetOverviewHTTPRouter(t *testing.T, source *runtimeObservationHTTPSource, now time.Time) (*gin.Engine, *runtimeobservation.Coordinator) {
	t.Helper()
	coordinator, err := runtimeobservation.New(source, runtimeobservation.Options{
		Now: func() time.Time { return now }, RefreshInterval: time.Hour, FreshnessWindow: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	fleet, err := fleetoverview.New(runtimeOverviewHTTPTopology{observations: coordinator}, runtimeOverviewHTTPRooms{}, runtimeOverviewHTTPRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	handler := &RuntimeObservabilityHandler{fleet: fleet, observations: coordinator}
	router := gin.New()
	router.GET("/api/v2/runtime-overview", handler.fleetOverview)
	return router, coordinator
}

func TestRuntimeObservationRefreshHTTPCollectsSelectedTarget(t *testing.T) {
	router, source, _ := newRuntimeObservationHTTPRouter(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v2/runtime-observations/actions/refresh", bytes.NewBufferString(`{"targetId":"agent:node"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"targetId":"agent:node"`) || !strings.Contains(response.Body.String(), `"state":"fresh"`) {
		t.Fatalf("status/body = %d: %s", response.Code, response.Body.String())
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.collects != 1 {
		t.Fatalf("collects=%d, want 1", source.collects)
	}
}

func TestRuntimeObservationRefreshHTTPReportsUnknownTarget(t *testing.T) {
	router, _, _ := newRuntimeObservationHTTPRouter(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v2/runtime-observations/actions/refresh", bytes.NewBufferString(`{"targetId":"agent:missing"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusNotFound, "RUNTIME_TARGET_NOT_FOUND")
}

func TestRuntimeObservationStreamHTTPStartsWithSnapshot(t *testing.T) {
	router, _, _ := newRuntimeObservationHTTPRouter(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v2/runtime-observations/stream?targetId=agent%3Anode", nil)
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
	if response.Header().Get("Cache-Control") != "no-cache, no-transform" || response.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("unexpected stream headers: %#v", response.Header())
	}
	body := response.Body.String()
	if !strings.Contains(body, "event: observation.snapshot") || !strings.Contains(body, `"targetId":"agent:node"`) || !strings.Contains(body, `"state":"fresh"`) {
		t.Fatalf("stream did not start with the selected target snapshot: %s", body)
	}
}

func TestFleetOverviewHTTPRefreshesSelectedTargetBeforeSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)
	stale := runtimeObservationHTTPInventory(now.Add(-2 * time.Minute))
	local := runtimeObservationHTTPInventory(now.Add(-2 * time.Minute))
	local.Target.ID, local.Target.AgentID, local.Target.Name = "local", "", "local"
	local.Target.Kind = agents.RuntimeKindLocal
	source := &runtimeObservationHTTPSource{items: []agents.RuntimeTargetInventory{stale, local}}
	source.collect = func(_ context.Context, selected agents.RuntimeTargetInventory) (agents.RuntimeTargetInventory, error) {
		fresh := runtimeObservationHTTPInventory(now)
		fresh.Target = selected.Target
		return fresh, nil
	}
	router, _ := newFleetOverviewHTTPRouter(t, source, now)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/runtime-overview?targetId=agent%3Anode", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, `"inventoryStale":true`) || !strings.Contains(body, `"observationState":"fresh"`) {
		t.Fatalf("overview did not use the inventory collected by the same request: %s", body)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.collects != 1 || len(source.collectedTargetIDs) != 1 || source.collectedTargetIDs[0] != "agent:node" {
		t.Fatalf("collected targets = %#v, collects = %d", source.collectedTargetIDs, source.collects)
	}
}

func TestFleetOverviewHTTPIncludesCollectionFailure(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)
	source := &runtimeObservationHTTPSource{items: []agents.RuntimeTargetInventory{runtimeObservationHTTPInventory(now)}}
	source.collect = func(context.Context, agents.RuntimeTargetInventory) (agents.RuntimeTargetInventory, error) {
		return agents.RuntimeTargetInventory{}, errors.New("I/O Operation Failed")
	}
	router, _ := newFleetOverviewHTTPRouter(t, source, now)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/runtime-overview?targetId=agent%3Anode", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"observationError":"I/O Operation Failed"`) {
		t.Fatalf("status/body = %d: %s", response.Code, response.Body.String())
	}
}

func TestFleetOverviewHTTPInventoryDetailRefreshesOnEveryRequest(t *testing.T) {
	now := time.Now().UTC()
	source := &runtimeObservationHTTPSource{items: []agents.RuntimeTargetInventory{runtimeObservationHTTPInventory(now)}}
	router, _ := newFleetOverviewHTTPRouter(t, source, now)
	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/runtime-overview?detail=inventory", nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"observationState":"fresh"`) {
			t.Fatalf("status/body = %d: %s", response.Code, response.Body.String())
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.collects != 2 {
		t.Fatalf("collects=%d, want 2 live inventory reads", source.collects)
	}
}

func TestFleetOverviewHTTPRejectsInvalidDetailBeforeCollection(t *testing.T) {
	now := time.Now().UTC()
	source := &runtimeObservationHTTPSource{items: []agents.RuntimeTargetInventory{runtimeObservationHTTPInventory(now)}}
	router, _ := newFleetOverviewHTTPRouter(t, source, now)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/runtime-overview?detail=other", nil))
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_RUNTIME_OVERVIEW_DETAIL")
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.collects != 0 {
		t.Fatalf("invalid request collected %d inventories", source.collects)
	}
}
