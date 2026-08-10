package maprenderer

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureLua(t *testing.T) []byte {
	t.Helper()
	tiles := append([]byte("VRSN\x00\x01\x00\x00\x00"), []byte{1, 0, 6, 0, 201, 0, 999 & 0xff, 999 >> 8}...)
	return []byte(`return {
  map = { width = 2, height = 2, tiles = "` + base64.StdEncoding.EncodeToString(tiles) + `" },
  ents = {
    walrus_camp = {{ x = -2, z = 2, data = { active = true } }},
    modded_crystal = {{ x = 2, z = -2, data = { custom = "kept", values = {1, 2} } }},
  },
  world_network = { persistdata = { clock = { cycles = 12, phase = "day" } } },
}`)
}

func TestDecodeSaveSupportsRawTextHeaderAndCompressedKlei(t *testing.T) {
	source := fixtureLua(t)
	compressed := new(bytes.Buffer)
	writer, err := flate.NewWriter(compressed, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(source); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	binaryPayload := append(make([]byte, 16), compressed.Bytes()...)
	klei := append([]byte("KLEI0001\x00\x00\x00"), []byte(base64.StdEncoding.EncodeToString(binaryPayload))...)

	for name, input := range map[string][]byte{
		"raw":            source,
		"textHeader":     append([]byte("KLEI     1\x00"), source...),
		"compressed":     klei,
		"binaryPreamble": append([]byte{3, 103, 136}, source...),
	} {
		t.Run(name, func(t *testing.T) {
			decoded, err := DecodeSave(bytes.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, source) {
				t.Fatalf("decoded source mismatch: %q", decoded[:min(len(decoded), 80)])
			}
		})
	}
}

func TestParsePreservesUnknownModFeaturesAndWorldState(t *testing.T) {
	parsed, err := Parse(context.Background(), fixtureLua(t))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TileWidth != 2 || parsed.TileHeight != 2 || len(parsed.TileIDs) != 4 || parsed.TileIDs[3] != 999 {
		t.Fatalf("parsed tiles = %#v (%dx%d)", parsed.TileIDs, parsed.TileWidth, parsed.TileHeight)
	}
	if len(parsed.Features) != 2 {
		t.Fatalf("features = %#v", parsed.Features)
	}
	var modded Feature
	for _, feature := range parsed.Features {
		if feature.Prefab == "modded_crystal" {
			modded = feature
		}
	}
	data, ok := modded.Properties["data"].(map[string]interface{})
	if !ok || data["custom"] != "kept" || modded.Category != "other" {
		t.Fatalf("modded feature = %#v", modded)
	}
	clock, ok := parsed.WorldState["clock"].(map[string]interface{})
	if !ok || clock["cycles"] != float64(12) {
		t.Fatalf("world state = %#v", parsed.WorldState)
	}
}

func TestParseSupportsGeneratedWorldSessionScript(t *testing.T) {
	tiles := append([]byte("VRSN\x00\x01\x00\x00\x00"), []byte{1, 0, 6, 0, 201, 0, 7, 0}...)
	fullScript := []byte(`local savedata = {}
local tablefunctions = {}
tablefunctions["map_fn"] = function()
return {width=2,height=2,tiles="` + base64.StdEncoding.EncodeToString(tiles) + `"}
end
savedata["map"] = tablefunctions["map_fn"]()
savedata["ents"] = {}
return savedata` + "\x00")
	parsed, err := Parse(context.Background(), bytes.TrimRight(fullScript, "\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TileWidth != 2 || parsed.TileHeight != 2 {
		t.Fatalf("dimensions = %dx%d", parsed.TileWidth, parsed.TileHeight)
	}
}

func TestParseHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := Parse(ctx, []byte("while true do end"))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestRendererWritesProtocolArtifacts(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "snapshot")
	output := filepath.Join(root, "output")
	if err := os.WriteFile(input, fixtureLua(t), 0440); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(output, 0750); err != nil {
		t.Fatal(err)
	}
	renderer := New("test-version")
	renderer.Now = func() time.Time { return time.Unix(123, 0) }
	manifest, err := renderer.Render(context.Background(), input, output)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ProtocolVersion != ProtocolVersion || manifest.RendererVersion != "test-version" || manifest.Statistics.FeatureCount != 2 || manifest.Statistics.UnknownTileCount != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}
	terrain, err := os.Open(filepath.Join(output, TerrainFileName))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(terrain)
	_ = terrain.Close()
	if err != nil || decoded.Bounds().Dx() != 8 || decoded.Bounds().Dy() != 8 {
		t.Fatalf("terrain bounds = %#v, %v", decoded.Bounds(), err)
	}
	if got := color.RGBAModel.Convert(decoded.At(0, 0)).(color.RGBA); got != terrainColors[201] {
		t.Fatalf("top-left tile = %#v, want %#v", got, terrainColors[201])
	}
	if got := color.RGBAModel.Convert(decoded.At(0, 7)).(color.RGBA); got != terrainColors[1] {
		t.Fatalf("bottom-left tile = %#v, want %#v", got, terrainColors[1])
	}
	for _, name := range []string{ManifestFileName, FeaturesFileName} {
		file, err := os.Open(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		var value interface{}
		decodeErr := json.NewDecoder(file).Decode(&value)
		_ = file.Close()
		if decodeErr != nil {
			t.Fatalf("decode %s: %v", name, decodeErr)
		}
	}
}
