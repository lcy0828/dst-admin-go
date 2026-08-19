package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/roomprovision"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type roomProvisionHTTPService struct {
	mu        sync.Mutex
	operation roomprovision.Operation
	roomID    string
	revision  string
	sourceJob string
	provision chan struct{}
	recover   chan struct{}
}

func (f *roomProvisionHTTPService) List(string) ([]roomprovision.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []roomprovision.Operation{f.operation}, nil
}

func (f *roomProvisionHTTPService) Get(string) (roomprovision.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.operation, nil
}

func (f *roomProvisionHTTPService) Provision(_ context.Context, roomID, revision, sourceJob string) (roomprovision.Operation, error) {
	f.mu.Lock()
	f.roomID, f.revision, f.sourceJob = roomID, revision, sourceJob
	f.operation.Status, f.operation.Phase = roomprovision.StatusSucceeded, "completed"
	f.mu.Unlock()
	select {
	case f.provision <- struct{}{}:
	default:
	}
	return f.operation, nil
}

func (f *roomProvisionHTTPService) RecoverOperation(context.Context, string) (roomprovision.Operation, error) {
	select {
	case f.recover <- struct{}{}:
	default:
	}
	return f.operation, nil
}

type roomProvisionHTTPCatalog struct{ room rooms.Room }

func (f roomProvisionHTTPCatalog) Room(id string) (rooms.Room, error) {
	if id != f.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return f.room, nil
}

func TestRoomProvisionHandlerSubmitsListsAndRecovers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "room_provision_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room-1", Name: "精确房间名"}
	service := &roomProvisionHTTPService{
		operation: roomprovision.Operation{
			ID: "00000000-0000-4000-8000-000000000001", RoomID: room.ID, RoomName: room.Name,
			TopologyRevision: "revision-1", Phase: "planned", Status: roomprovision.StatusRunning,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			Steps: []roomprovision.Step{{WorldID: "master", WorldName: "地面"}},
		},
		provision: make(chan struct{}, 1), recover: make(chan struct{}, 1),
	}
	handler, err := NewRoomProvisionHandler(service, roomProvisionHTTPCatalog{room: room}, jobService)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	path := "/api/v2/rooms/room-1/topology/actions/provision"
	response := performJSON(router, http.MethodPost, path, map[string]string{
		"expectedRevision": "revision-1", "confirmation": "错误房间名",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodPost, path, map[string]string{
		"expectedRevision": "revision-1", "confirmation": room.Name,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	select {
	case <-service.provision:
	case <-time.After(2 * time.Second):
		t.Fatal("provision job did not invoke the coordinator")
	}
	service.mu.Lock()
	if service.roomID != room.ID || service.revision != "revision-1" || service.sourceJob == "" {
		t.Fatalf("provision arguments=%q %q %q", service.roomID, service.revision, service.sourceJob)
	}
	service.mu.Unlock()

	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room-1/provision-operations", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodPost, "/api/v2/provision-operations/00000000-0000-4000-8000-000000000001/actions/recover", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	select {
	case <-service.recover:
	case <-time.After(2 * time.Second):
		t.Fatal("recover job did not invoke the coordinator")
	}
}
