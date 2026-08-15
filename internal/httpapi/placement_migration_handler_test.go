package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/placementmigration"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type migrationHandlerRooms struct {
	room  rooms.Room
	world rooms.World
}

func (c migrationHandlerRooms) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}

func (c migrationHandlerRooms) World(roomID, worldID string) (rooms.World, error) {
	if roomID != c.room.ID || worldID != c.world.ID {
		return rooms.World{}, rooms.ErrWorldNotFound
	}
	return c.world, nil
}

type migrationHandlerCoordinator struct {
	mu       sync.Mutex
	roomID   string
	worldID  string
	revision string
	called   chan struct{}
}

func (c *migrationHandlerCoordinator) Migrate(_ context.Context, roomID, worldID, revision string) (placementmigration.Result, error) {
	c.mu.Lock()
	c.roomID, c.worldID, c.revision = roomID, worldID, revision
	c.mu.Unlock()
	select {
	case c.called <- struct{}{}:
	default:
	}
	return placementmigration.Result{TargetTargetID: "agent:node", BytesTransferred: 42, RecoveryRef: "recovery/ref"}, nil
}

func TestPlacementMigrationHandlerRequiresConfirmationAndSubmitsJob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "placement_migration_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room", Name: "精确房间名"}
	world := rooms.World{ID: "world", RoomID: room.ID, Name: "地表"}
	coordinator := &migrationHandlerCoordinator{called: make(chan struct{}, 1)}
	handler, err := NewPlacementMigrationHandler(coordinator, migrationHandlerRooms{room: room, world: world}, jobService)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	path := "/api/v2/rooms/room/topology/actions/apply"
	response := performJSON(router, http.MethodPost, path, map[string]string{
		"worldId": world.ID, "expectedRevision": "revision-1", "confirmation": "错误",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodPost, path, map[string]string{
		"worldId": world.ID, "expectedRevision": "revision-1", "confirmation": room.Name,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	select {
	case <-coordinator.called:
	case <-time.After(2 * time.Second):
		t.Fatal("migration job did not call coordinator")
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.roomID != room.ID || coordinator.worldID != world.ID || coordinator.revision != "revision-1" {
		t.Fatalf("migration arguments=%q %q %q", coordinator.roomID, coordinator.worldID, coordinator.revision)
	}
}
