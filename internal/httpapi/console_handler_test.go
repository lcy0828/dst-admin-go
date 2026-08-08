package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	consoleapi "dont/internal/console"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type consoleHandlerRooms struct{}

func (consoleHandlerRooms) Room(id string) (rooms.Room, error) {
	return rooms.Room{ID: id, DirectoryName: "room", Name: "周末服", Managed: true}, nil
}

func (consoleHandlerRooms) World(roomID, worldID string) (rooms.World, error) {
	return rooms.World{ID: worldID, RoomID: roomID, DirectoryName: "Master", Name: "Master"}, nil
}

type consoleHandlerSender struct{}

func (consoleHandlerSender) Send(context.Context, string, string, string) error { return nil }

func newConsoleHandlerRouter(t *testing.T) *gin.Engine {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := consoleapi.NewStore(db, "console_http_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := consoleapi.NewService(consoleHandlerRooms{}, consoleHandlerSender{}, store)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewConsoleHandler(service).Register(router.Group("/api/v2"))
	return router
}

func TestConsoleCommandDefinitionAndHistoryRoutes(t *testing.T) {
	router := newConsoleHandlerRouter(t)
	response := performJSON(router, http.MethodGet, "/api/v2/commands", nil, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("list commands: %d %s", response.Code, response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/commands", map[string]interface{}{
		"name": "测试公告", "description": "真实持久化", "category": "自定义命令",
		"script":     `c_announce("{message}")`,
		"parameters": []map[string]interface{}{{"name": "message", "label": "内容", "type": "string", "required": true}},
	}, nil, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("create command: %d %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data consoleapi.Definition `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.ID == "" {
		t.Fatalf("decode command: %#v %v", envelope, err)
	}
	response = performJSON(router, http.MethodGet, "/api/v2/commands/"+envelope.Data.ID, nil, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get command: %d %s", response.Code, response.Body.String())
	}
	response = performJSON(router, http.MethodDelete, "/api/v2/commands/save_world", nil, nil, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("delete builtin: %d %s", response.Code, response.Body.String())
	}
	response = performJSON(router, http.MethodDelete, "/api/v2/commands/"+envelope.Data.ID, nil, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("delete command: %d %s", response.Code, response.Body.String())
	}
	response = performJSON(router, http.MethodDelete, "/api/v2/rooms/room/command-runs", nil, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("clear history: %d %s", response.Code, response.Body.String())
	}
}
