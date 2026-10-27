package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	consoleapi "dont/internal/console"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"errors"
	"time"

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

func newConsoleHandlerRouter(t *testing.T, commanders ...consoleapi.RuntimeCommander) *gin.Engine {
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
	service, err := consoleapi.NewService(consoleHandlerRooms{}, consoleHandlerSender{}, store, commanders...)
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

type handlerLateCommander struct{ reads int }

func (c *handlerLateCommander) ExecuteCommand(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	return dstruntime.CommandReceipt{}, errors.Join(dstruntime.ErrRuntimeCommandUnconfirmed, dstruntime.ErrRuntimeResultAbsent)
}
func (c *handlerLateCommander) ReadCommandReceipt(_ context.Context, room, world string, r dstruntime.CommandRequest, since time.Time) (dstruntime.CommandReceipt, error) {
	c.reads++
	return dstruntime.CommandReceipt{RequestID: r.RequestID, Action: r.Action, OK: true, Code: "COMMAND_EXECUTED", CompletedAt: time.Now(), Details: map[string]interface{}{"output": map[string]interface{}{"text": "0\n", "truncated": false}}}, nil
}
func TestConsoleLateReceiptHTTPIsOptInAndRoomScoped(t *testing.T) {
	commander := &handlerLateCommander{}
	router := newConsoleHandlerRouter(t, commander)
	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room/worlds/Master/raw-commands", map[string]interface{}{"command": "print(0)", "confirmation": "周末服"}, nil, "")
	var envelope struct {
		Data consoleapi.Run `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.Status != consoleapi.RunUncertain {
		t.Fatalf("%d %s %v", response.Code, response.Body.String(), err)
	}
	id := envelope.Data.ID
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/v2/rooms/room/command-runs/" + id, 200},
		{"/api/v2/rooms/other/command-runs/" + id + "?refresh=true", 404},
		{"/api/v2/rooms/room/command-runs/" + id + "?refresh=invalid", 400},
	} {
		response = performJSON(router, http.MethodGet, tc.path, nil, nil, "")
		if response.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, response.Code, response.Body.String())
		}
	}
	if commander.reads != 0 {
		t.Fatal("lookup ran before explicit scoped request")
	}
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/command-runs/"+id+"?refresh=true", nil, nil, "")
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.Status != consoleapi.RunSucceeded || envelope.Data.ResultState != "output" || envelope.Data.Output.Text != "0\n" || envelope.Data.ReceiptCheck.Status != "found" {
		t.Fatalf("%d %s %v", response.Code, response.Body.String(), err)
	}
	if response.Header().Get("Cache-Control") != "no-store" || commander.reads != 1 {
		t.Fatal("lookup cached or duplicated")
	}
}
