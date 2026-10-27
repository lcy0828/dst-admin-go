package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/aicontrol"
	consoleapi "dont/internal/console"
	"dont/internal/dstruntime"
	"dont/internal/jobs"
	"dont/internal/shards"
	"github.com/jinzhu/gorm"
)

func TestAIBackendUsesRoomHandlersAndJobReceipts(t *testing.T) {
	router, taskService, control, roomService := newRoomHandlerApp(t)
	created := performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]any{"directoryName": "ai-room", "name": "AI 测试房间", "gameMode": "survival", "maxPlayers": 6, "includeCaves": true}, nil, "")
	assertStatus(t, created, 201)
	roomID := responseData(t, created)["id"].(string)
	handler := NewRoomHandler(roomService, shards.NewOperations(roomService, control), taskService)
	backend := NewAIBackend(handler, nil, nil, nil, nil, nil, NewJobHandler(taskService), nil, false)
	selectedRoom := roomID
	ctx := context.Background()
	data, err := backend.Invoke(ctx, "runtime_modes", aicontrol.Arguments{RoomID: roomID}, selectedRoom)
	if err != nil || !strings.Contains(string(data), "game") {
		t.Fatal(string(data), err)
	}
	data, err = backend.Invoke(ctx, "room_control", aicontrol.Arguments{RoomID: roomID, Action: "start", RuntimeMode: "game"}, selectedRoom)
	if err != nil {
		t.Fatal(err)
	}
	var task jobs.Job
	if err := json.Unmarshal(data, &task); err != nil || task.ID == "" {
		t.Fatal(string(data), err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		value, _ := taskService.Get(task.ID)
		if value.Status == jobs.StatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err = backend.Invoke(ctx, "get_job", aicontrol.Arguments{RoomID: roomID, JobID: task.ID}, selectedRoom)
	if err != nil || !strings.Contains(string(data), "succeeded") {
		t.Fatal(string(data), err)
	}
	if _, err := backend.Invoke(ctx, "get_job", aicontrol.Arguments{RoomID: roomID, JobID: task.ID}, "other-room"); !errors.Is(err, aicontrol.ErrDenied) {
		t.Fatal("task room scope bypass", err)
	}
	if _, err := backend.Invoke(ctx, "get_job", aicontrol.Arguments{RoomID: "other-room", JobID: task.ID}, ""); !errors.Is(err, aicontrol.ErrDenied) {
		t.Fatal("task room did not match the requested room", err)
	}
	backend.member = true
	if _, err := backend.Invoke(ctx, "room_control", aicontrol.Arguments{RoomID: roomID, Action: "stop"}, selectedRoom); !errors.Is(err, aicontrol.ErrDenied) {
		t.Fatal("managed member accepted writes", err)
	}
}

func TestAIBackendWorldStateFiltersRequestedWorld(t *testing.T) {
	backend := &AIBackend{states: NewWorldStateHandler(worldStateHandlerService{})}
	data, err := backend.Invoke(context.Background(), "world_state", aicontrol.Arguments{RoomID: "room", WorldID: "master"}, "")
	if err != nil || !strings.Contains(string(data), `"worldId":"master"`) || strings.Contains(string(data), `"items"`) {
		t.Fatal(string(data), err)
	}
	if _, err := backend.Invoke(context.Background(), "world_state", aicontrol.Arguments{RoomID: "room", WorldID: "missing"}, ""); err == nil || !strings.Contains(err.Error(), "NOT_FOUND") {
		t.Fatal("missing world not surfaced", err)
	}
}

type aiOutputCommander struct{ calls int }

func (c *aiOutputCommander) ExecuteCommand(_ context.Context, _, _ string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	c.calls++
	return dstruntime.CommandReceipt{RequestID: request.RequestID, Action: request.Action, OK: true, CompletedAt: time.Now().UTC(), Details: map[string]interface{}{"output": map[string]interface{}{"text": strings.Repeat("<", 16*1024), "truncated": false}}}, nil
}

func TestAIConsolePreservesSuccessfulLargeReceipt(t *testing.T) {
	router, _, _, roomService := newRoomHandlerApp(t)
	created := performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]any{"directoryName": "review-room", "name": "Review Room", "gameMode": "survival", "maxPlayers": 6, "includeCaves": true}, nil, "")
	assertStatus(t, created, 201)
	roomID := responseData(t, created)["id"].(string)
	worlds, err := roomService.Worlds(roomID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	db.LogMode(false)
	commandStore := consoleapi.NewStore(db, "review_")
	if err := commandStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	commander := &aiOutputCommander{}
	console, err := consoleapi.NewService(roomService, consoleHandlerSender{}, commandStore, commander)
	if err != nil {
		t.Fatal(err)
	}
	aiStore := aicontrol.NewStore(db, "review_", t.TempDir())
	if err := aiStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := aiStore.SaveConfig(aicontrol.ConfigInput{Config: aicontrol.Config{Revision: "initial", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	backend := NewAIBackend(&RoomHandler{rooms: roomService}, nil, nil, nil, nil, nil, nil, NewConsoleHandler(console), false)
	ai := aicontrol.New(aiStore, backend)
	result, err := ai.Invoke(context.Background(), aicontrol.Principal{ID: "page", RoomID: roomID}, "world_console", json.RawMessage(fmt.Sprintf(`{"roomId":%q,"worldId":%q,"command":"c_save(); print(string.rep('<',16384))","requestId":"review-real-console"}`, roomID, worlds[0].ID)))
	if err != nil {
		t.Fatal(err)
	}
	runs, _, err := console.Runs(consoleapi.ListFilter{RoomID: roomID})
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	if commander.calls != 1 || runs[0].Status != consoleapi.RunSucceeded || result.Status != "succeeded" || !strings.Contains(string(result.Data), runs[0].ID) || len(result.Data) > 64<<10 || !strings.Contains(string(result.Data), "_aiTruncated") {
		t.Error("AI lost a real successful console receipt", commander.calls, runs[0].Status, result.Status)
	}
}

func TestAILateReceiptLookupUsesSameConsoleResult(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	commandStore := consoleapi.NewStore(db, "late_ai_")
	if err = commandStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	commander := &handlerLateCommander{}
	service, err := consoleapi.NewService(consoleHandlerRooms{}, consoleHandlerSender{}, commandStore, commander)
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.ExecuteRaw(context.Background(), "room", "Master", consoleapi.RawRequest{Command: "print(0)", Confirmation: "周末服"})
	if err != nil {
		t.Fatal(err)
	}
	backend := &AIBackend{console: NewConsoleHandler(service)}
	aiStore := aicontrol.NewStore(db, "late_ai_", t.TempDir())
	if err = aiStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err = aiStore.SaveConfig(aicontrol.ConfigInput{Config: aicontrol.Config{Revision: "initial", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	ai := aicontrol.New(aiStore, backend)
	result, err := ai.Invoke(context.Background(), aicontrol.Principal{ID: "mcp", RoomID: "room"}, "command_receipt", json.RawMessage(fmt.Sprintf(`{"roomId":"room","runId":%q,"refresh":true}`, run.ID)))
	if err != nil {
		t.Fatal(err)
	}
	var got consoleapi.Run
	if err = json.Unmarshal(result.Data, &got); err != nil || got.Status != consoleapi.RunSucceeded || got.Output.Text != "0\n" || got.ReceiptCheck.Status != "found" || commander.reads != 1 {
		t.Fatalf("result=%s err=%v reads=%d", result.Data, err, commander.reads)
	}
}
