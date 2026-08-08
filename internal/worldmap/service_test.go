package worldmap

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dont/internal/rooms"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type mapCatalog struct {
	room  rooms.Room
	world rooms.World
}

func (c mapCatalog) Room(string) (rooms.Room, error)        { return c.room, nil }
func (c mapCatalog) World(_, _ string) (rooms.World, error) { return c.world, nil }

type failingRenderer struct{ err error }

func (r failingRenderer) Available() (bool, string) { return true, "failing" }
func (r failingRenderer) Render(context.Context, string, string, []Layer, io.Writer) error {
	return r.err
}

func newMapService(t *testing.T, renderer Renderer, retention int) (*Service, string, string) {
	t.Helper()
	saveRoot := filepath.Join(t.TempDir(), "saves")
	mapRoot := filepath.Join(t.TempDir(), "maps")
	sessionRoot := filepath.Join(saveRoot, "Cluster", "Master", "save", "session", "ABC123")
	if err := os.MkdirAll(filepath.Join(sessionRoot, "KU_PLAYER_"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionRoot, "0000000001"), []byte("return { map = {} }"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionRoot, "0000000001.meta"), []byte("meta"), 0640); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Test", Managed: true}
	world := rooms.World{ID: "world", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	if renderer == nil {
		renderer = NewMemoryRenderer()
	}
	service, err := NewService(Config{SaveRoot: saveRoot, MapRoot: mapRoot, Retention: retention}, mapCatalog{room: room, world: world}, store, renderer)
	if err != nil {
		t.Fatal(err)
	}
	return service, sessionRoot, mapRoot
}

func TestSessionsDiscoverLatestSnapshotsAndIgnoreMetadataAndSymlinks(t *testing.T) {
	service, sessionRoot, _ := newMapService(t, nil, 3)
	second := filepath.Join(sessionRoot, "0000000002")
	if err := os.WriteFile(second, []byte("new"), 0640); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Minute)
	if err := os.Chtimes(second, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sessionRoot, "0000000001"), filepath.Join(sessionRoot, "linked")); err != nil {
		t.Fatal(err)
	}
	items, err := service.Sessions("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].FileName != "0000000002" || !items[0].Latest || items[1].Latest || items[0].PlayerCount != 1 {
		t.Fatalf("sessions = %#v", items)
	}
	file, info, opened, err := service.OpenSession(items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if info.Size() != 3 || opened.SessionID != "ABC123" {
		t.Fatalf("opened = %#v, size = %d", opened, info.Size())
	}
}

func TestGenerateValidatesLayersPublishesAtomicallyAndPrunes(t *testing.T) {
	service, _, mapRoot := newMapService(t, nil, 1)
	sessions, err := service.Sessions("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	layers, err := normalizeLayers([]Layer{LayerPlayers, LayerTerrain, LayerPlayers})
	if err != nil || !reflect.DeepEqual(layers, []Layer{LayerTerrain, LayerPlayers}) {
		t.Fatalf("layers = %#v, %v", layers, err)
	}
	first, err := service.Generate(context.Background(), "job-1", "room", "world", sessions[0].ID, layers)
	if err != nil || first.Status != "succeeded" || first.Width != 960 || first.Height != 640 {
		t.Fatalf("first map = %#v, %v", first, err)
	}
	file, _, _, err := service.OpenImage(first.ID, LayerPlayers)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	second, err := service.Generate(context.Background(), "job-2", "room", "world", sessions[0].ID, layers)
	if err != nil {
		t.Fatal(err)
	}
	items, err := service.List("room")
	if err != nil || len(items) != 1 || items[0].ID != second.ID {
		t.Fatalf("maps = %#v, %v", items, err)
	}
	if _, err := os.Stat(filepath.Join(mapRoot, first.ID)); !os.IsNotExist(err) {
		t.Fatalf("old map directory still exists: %v", err)
	}
}

func TestFailedGenerationKeepsPreviousMapAndPersistsFailureStage(t *testing.T) {
	service, _, mapRoot := newMapService(t, nil, 3)
	sessions, _ := service.Sessions("room", "world")
	previous, err := service.Generate(context.Background(), "job-ok", "room", "world", sessions[0].ID, []Layer{LayerTerrain})
	if err != nil {
		t.Fatal(err)
	}
	service.renderer = failingRenderer{err: errors.New("unsupported save revision")}
	if _, err := service.Generate(context.Background(), "job-fail", "room", "world", sessions[0].ID, []Layer{LayerTerrain}); err == nil {
		t.Fatal("failed renderer was accepted")
	}
	items, err := service.List("room")
	if err != nil || len(items) != 2 || items[0].Status != "failed" || items[0].Stage != "renderer" || !strings.Contains(items[0].ErrorMessage, "unsupported save revision") {
		t.Fatalf("maps = %#v, %v", items, err)
	}
	if _, err := os.Stat(filepath.Join(mapRoot, previous.ID, "terrain.png")); err != nil {
		t.Fatalf("previous map was removed: %v", err)
	}
}

func TestFailedGenerationRecordsAreBounded(t *testing.T) {
	service, _, _ := newMapService(t, failingRenderer{err: errors.New("unsupported save revision")}, 3)
	sessions, err := service.Sessions("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < failedRetention+2; index++ {
		if _, err := service.Generate(context.Background(), "job-fail", "room", "world", sessions[0].ID, []Layer{LayerTerrain}); err == nil {
			t.Fatal("failed renderer was accepted")
		}
	}
	items, err := service.store.ByStatus("room", "world", "failed")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != failedRetention {
		t.Fatalf("failed records = %d, want %d", len(items), failedRetention)
	}
}

func TestCleanupStagingOnlyRemovesRendererTemporaryEntries(t *testing.T) {
	root := t.TempDir()
	staleDirectory := filepath.Join(root, ".map-render-stale")
	staleFile := filepath.Join(root, ".map-render-file")
	completedMap := filepath.Join(root, "86fc8959-76cf-495a-a13b-275628eb27de")
	for _, directory := range []string{staleDirectory, completedMap} {
		if err := os.MkdirAll(directory, 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(staleFile, []byte("stale"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaging(root); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{staleDirectory, staleFile} {
		if _, err := os.Lstat(removed); !os.IsNotExist(err) {
			t.Fatalf("staging entry still exists: %s (%v)", removed, err)
		}
	}
	if _, err := os.Stat(completedMap); err != nil {
		t.Fatalf("completed map was removed: %v", err)
	}
}

func TestPrepareRejectsInvalidLayersAndConcurrentWorldGeneration(t *testing.T) {
	service, _, _ := newMapService(t, nil, 3)
	sessions, _ := service.Sessions("room", "world")
	if _, _, _, err := service.Prepare("room", GenerateRequest{WorldID: "world", SessionID: sessions[0].ID, Layers: []Layer{"bad"}}); !errors.Is(err, ErrInvalidLayers) {
		t.Fatalf("invalid layers error = %v", err)
	}
	_, _, release, err := service.Prepare("room", GenerateRequest{WorldID: "world", SessionID: sessions[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.Prepare("room", GenerateRequest{WorldID: "world", SessionID: sessions[0].ID}); !errors.Is(err, ErrGenerationInProgress) {
		t.Fatalf("concurrent error = %v", err)
	}
	release()
}

func TestExecRendererUsesArgumentArray(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "renderer")
	argumentsFile := filepath.Join(root, "arguments")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DST_MAP_TEST_ARGUMENTS\"\n"
	if err := os.WriteFile(executable, []byte(script), 0750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DST_MAP_TEST_ARGUMENTS", argumentsFile)
	renderer := NewExecRenderer(executable)
	input := filepath.Join(root, "input;touch-not-executed")
	output := filepath.Join(root, "output with spaces")
	if err := renderer.Render(context.Background(), input, output, []Layer{LayerTerrain, LayerPlayers}, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argumentsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	expected := []string{"--input", input, "--output", output, "--layers", "terrain,players"}
	if !reflect.DeepEqual(lines, expected) {
		t.Fatalf("arguments = %#v", lines)
	}
}
