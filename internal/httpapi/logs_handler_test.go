package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/logstream"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type logHandlerCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c logHandlerCatalog) Room(string) (rooms.Room, error)      { return c.room, nil }
func (c logHandlerCatalog) Worlds(string) ([]rooms.World, error) { return c.worlds, nil }
func (c logHandlerCatalog) World(_ string, worldID string) (rooms.World, error) {
	for _, world := range c.worlds {
		if world.ID == worldID {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

func TestRoomLogSnapshotReturnsPartialWorldResults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	room := rooms.Room{ID: rooms.EncodeID("room"), DirectoryName: "room", Name: "测试房间"}
	worlds := []rooms.World{
		{ID: rooms.EncodeID("Master"), DirectoryName: "Master", Name: "地面", Role: rooms.WorldRoleMaster},
		{ID: rooms.EncodeID("Caves"), DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves},
	}
	for _, world := range worlds {
		if err := os.MkdirAll(filepath.Join(root, room.DirectoryName, world.DirectoryName), 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, room.DirectoryName, "Master", "server_log.txt"), []byte("ready\n"), 0640); err != nil {
		t.Fatal(err)
	}
	service, err := logstream.NewService(root, logHandlerCatalog{room: room, worlds: worlds})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewLogHandler(service).Register(v2)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v2/rooms/"+room.ID+"/logs?limit=20", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data logstream.RoomSnapshot `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Data.Partial || envelope.Data.Available != 1 || envelope.Data.Unavailable != 1 ||
		len(envelope.Data.Worlds) != 2 || envelope.Data.Worlds[1].Problem == nil || envelope.Data.Worlds[1].Problem.Code != "LOG_NOT_FOUND" {
		t.Fatalf("room log response = %#v", envelope.Data)
	}
}
