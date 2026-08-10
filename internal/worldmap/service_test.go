package worldmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dont/internal/maprenderer"
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
	layers, err := normalizeLayers([]Layer{LayerWorldState, LayerTerrain, LayerFeatures, LayerTerrain})
	if err != nil || !reflect.DeepEqual(layers, []Layer{LayerTerrain, LayerFeatures, LayerWorldState}) {
		t.Fatalf("layers = %#v, %v", layers, err)
	}
	first, err := service.Generate(context.Background(), "job-1", "room", "world", sessions[0].ID, layers)
	if err != nil || first.Status != "succeeded" || first.Width != 960 || first.Height != 640 {
		t.Fatalf("first map = %#v, %v", first, err)
	}
	file, _, _, err := service.OpenImage(first.ID, LayerTerrain)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	for _, artifact := range []Artifact{ArtifactManifest, ArtifactFeatures} {
		file, _, _, err := service.OpenArtifact(first.ID, artifact)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	if first.SourceSHA256 == "" || first.RendererVersion != "test" || !reflect.DeepEqual(first.Layers, []Layer{LayerTerrain, LayerFeatures, LayerWorldState}) {
		t.Fatalf("renderer metadata = %#v", first)
	}
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
	for _, legacy := range []Layer{"walrusCamps", "spawnPoints", "players"} {
		if _, _, _, err := service.Prepare("room", GenerateRequest{WorldID: "world", SessionID: sessions[0].ID, Layers: []Layer{legacy}}); !errors.Is(err, ErrInvalidLayers) {
			t.Fatalf("legacy layer %q error = %v", legacy, err)
		}
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
	script := "#!/bin/sh\nif [ \"$1\" = \"--probe\" ]; then\n  printf '%s\\n' '{\"protocolVersion\":\"1\",\"rendererVersion\":\"test\",\"capabilities\":{\"inputFormats\":[\"session\"],\"artifacts\":[\"terrain.png\",\"manifest.json\",\"features.json\"],\"maxInputSize\":1}}'\n  exit 0\nfi\nprintf '%s\\n' \"$@\" > \"$DST_MAP_TEST_ARGUMENTS\"\n"
	if err := os.WriteFile(executable, []byte(script), 0750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DST_MAP_TEST_ARGUMENTS", argumentsFile)
	renderer := NewExecRenderer(executable)
	input := filepath.Join(root, "input;touch-not-executed")
	output := filepath.Join(root, "output with spaces")
	if err := os.Mkdir(output, 0750); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Render(context.Background(), input, output, []Layer{LayerTerrain, LayerFeatures}, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argumentsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	expected := []string{"--input", input, "--output", output, "--layers", "terrain,features"}
	if !reflect.DeepEqual(lines, expected) {
		t.Fatalf("arguments = %#v", lines)
	}
}

func TestRendererProbeAllowsAdditiveCapabilitiesAndRejectsIncompatibleProtocol(t *testing.T) {
	root := t.TempDir()
	compatible := filepath.Join(root, "compatible")
	compatibleScript := "#!/bin/sh\nprintf '%s\\n' '{\"protocolVersion\":\"1\",\"rendererVersion\":\"future\",\"capabilities\":{\"inputFormats\":[\"session\"],\"artifacts\":[\"terrain.png\",\"manifest.json\",\"features.json\"],\"maxInputSize\":1,\"futureField\":true},\"futureRoot\":true}'\n"
	if err := os.WriteFile(compatible, []byte(compatibleScript), 0750); err != nil {
		t.Fatal(err)
	}
	info := probeRenderer(context.Background(), compatible)
	if !info.Available || info.Version != "future" {
		t.Fatalf("compatible probe = %#v", info)
	}

	incompatible := filepath.Join(root, "incompatible")
	incompatibleScript := "#!/bin/sh\nprintf '%s\\n' '{\"protocolVersion\":\"2\",\"rendererVersion\":\"future\",\"capabilities\":{\"artifacts\":[\"terrain.png\",\"manifest.json\",\"features.json\"]}}'\n"
	if err := os.WriteFile(incompatible, []byte(incompatibleScript), 0750); err != nil {
		t.Fatal(err)
	}
	info = probeRenderer(context.Background(), incompatible)
	if info.Available || !strings.Contains(info.Error, "incompatible") {
		t.Fatalf("incompatible probe = %#v", info)
	}
}

func TestRendererArtifactsRejectExtraFilesAndHashMismatch(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "session")
	if err := os.WriteFile(input, []byte("session snapshot"), 0440); err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256([]byte("session snapshot"))
	expectedSHA256 := hex.EncodeToString(expectedHash[:])

	render := func() string {
		output := filepath.Join(root, time.Now().Format("150405.000000000"))
		if err := os.Mkdir(output, 0750); err != nil {
			t.Fatal(err)
		}
		if err := NewMemoryRenderer().Render(context.Background(), input, output, []Layer{LayerTerrain}, io.Discard); err != nil {
			t.Fatal(err)
		}
		return output
	}

	valid := render()
	if _, err := validateRendererArtifacts(valid, expectedSHA256); err != nil {
		t.Fatalf("valid artifacts rejected: %v", err)
	}
	extra := render()
	if err := os.WriteFile(filepath.Join(extra, "unexpected.txt"), []byte("unexpected"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRendererArtifacts(extra, expectedSHA256); !errors.Is(err, ErrRendererOutput) {
		t.Fatalf("extra artifact error = %v", err)
	}
	mismatch := render()
	if _, err := validateRendererArtifacts(mismatch, strings.Repeat("0", 64)); !errors.Is(err, ErrRendererOutput) {
		t.Fatalf("hash mismatch error = %v", err)
	}
}

func TestCopySessionSnapshotAndRendererTextSanitization(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "Cluster", "Master", "session")
	if err := os.MkdirAll(filepath.Dir(source), 0750); err != nil {
		t.Fatal(err)
	}
	content := []byte("immutable Session")
	if err := os.WriteFile(source, content, 0640); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "snapshot")
	digest, err := copySessionSnapshot(context.Background(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(content)
	if digest != hex.EncodeToString(expected[:]) {
		t.Fatalf("snapshot hash = %q", digest)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm()&0222 != 0 {
		t.Fatalf("snapshot mode = %v, error = %v", info.Mode(), err)
	}

	raw := "\x1b[31mfailed " + source + " token=abc123 password:secret\x00\x1b[0m"
	cleaned := sanitizeRendererText(raw, source, root)
	for _, forbidden := range []string{source, root, "abc123", "secret", "\x1b", "\x00"} {
		if strings.Contains(cleaned, forbidden) {
			t.Fatalf("sanitized text still contains %q: %q", forbidden, cleaned)
		}
	}
	if !strings.Contains(cleaned, "token=[REDACTED]") || !strings.Contains(cleaned, "password=[REDACTED]") {
		t.Fatalf("sanitized text = %q", cleaned)
	}
}

func TestMemoryRendererManifestUsesProtocolV1(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "session")
	output := filepath.Join(root, "output")
	if err := os.WriteFile(input, []byte("snapshot"), 0440); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(output, 0750); err != nil {
		t.Fatal(err)
	}
	if err := NewMemoryRenderer().Render(context.Background(), input, output, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	var manifest maprenderer.Manifest
	if err := decodeArtifactJSON(filepath.Join(output, maprenderer.ManifestFileName), maxManifestSize, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ProtocolVersion != maprenderer.ProtocolVersion || manifest.SourceSHA256 == "" || manifest.Map.WorldBounds.MaxX <= manifest.Map.WorldBounds.MinX {
		t.Fatalf("manifest = %#v", manifest)
	}
}
