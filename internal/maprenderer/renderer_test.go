package maprenderer

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureAssets(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data")
	for _, directory := range []string{"databundles", "levels/tiles", "levels/textures", "minimap"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0750); err != nil {
			t.Fatal(err)
		}
	}
	constants := "GROUND = {\n IMPASSABLE = 1,\n GRASS = 6,\n OCEAN_COASTAL = 201,\n}\nGROUND_NAMES = {}\n"
	tiledefs := `local TileManager = require("tilemanager")
TileManager.RegisterTileRange("LAND", 1, 10)
TileManager.AddTile("IMPASSABLE", "LAND", {old_static_id=GROUND.IMPASSABLE}, {}, {noise_texture="mini_impassable"})
TileManager.AddTile("GRASS", "LAND", {old_static_id=GROUND.GRASS}, {}, {noise_texture="mini_grass_noise"})
TileManager.AddTile("OCEAN_COASTAL", "OCEAN", {old_static_id=GROUND.OCEAN_COASTAL}, {}, {noise_texture="ocean_noise"})
`
	writeFixtureZip(t, filepath.Join(root, "databundles", "scripts.zip"), map[string][]byte{
		"scripts/constants.lua": []byte(constants), "scripts/tiledefs.lua": []byte(tiledefs),
	})
	writeFixtureZip(t, filepath.Join(root, "databundles", "images.zip"), nil)
	writeFixtureKTEX(t, filepath.Join(root, "levels", "textures", "mini_impassable.tex"), 4, 4, color.NRGBA{R: 40, G: 40, B: 40, A: 255}, nil)
	writeFixtureKTEX(t, filepath.Join(root, "levels", "textures", "mini_grass_noise.tex"), 4, 4, color.NRGBA{R: 80, G: 160, B: 60, A: 255}, nil)
	writeFixtureKTEX(t, filepath.Join(root, "levels", "textures", "ocean_noise.tex"), 4, 4, color.NRGBA{R: 30, G: 90, B: 130, A: 255}, nil)
	edgeAlpha := func(x, y int) uint8 {
		if x < 4 && y < 4 {
			return 255
		}
		return 0
	}
	writeFixtureKTEX(t, filepath.Join(root, "levels", "tiles", "map_edge.tex"), 32, 24, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, edgeAlpha)
	var atlas strings.Builder
	atlas.WriteString(`<Atlas><Texture filename="map_edge.tex"/><Elements>`)
	for number := 1; number <= 48; number++ {
		x, y := (number-1)%8, (number-1)/8
		fmt.Fprintf(&atlas, `<Element name="%02d" u1="%g" u2="%g" v1="%g" v2="%g"/>`, number, float64(x)/8, float64(x+1)/8, float64(y)/6, float64(y+1)/6)
	}
	atlas.WriteString(`</Elements></Atlas>`)
	if err := os.WriteFile(filepath.Join(root, "levels", "tiles", "map_edge.xml"), []byte(atlas.String()), 0640); err != nil {
		t.Fatal(err)
	}
	writeFixtureKTEX(t, filepath.Join(root, "minimap", "minimap_atlas.tex"), 4, 4, color.NRGBA{A: 255}, nil)
	if err := os.WriteFile(filepath.Join(root, "minimap", "minimap_data.xml"), []byte(`<Atlas><Texture filename="minimap_atlas.tex"/><Elements><Element name="portal_dst.png" u1="0" u2="1" v1="0" v2="1"/></Elements></Atlas>`), 0640); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFixtureZip(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, data := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeFixtureKTEX(t *testing.T, path string, width, height int, shade color.NRGBA, alpha func(int, int) uint8) {
	t.Helper()
	pixels := make([]byte, width*height*4)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value := shade
			if alpha != nil {
				value.A = alpha(x, y)
			}
			offset := (y*width + x) * 4
			copy(pixels[offset:offset+4], []byte{value.R, value.G, value.B, value.A})
		}
	}
	data := make([]byte, 18+len(pixels))
	copy(data, "KTEX")
	binary.LittleEndian.PutUint32(data[4:8], uint32(4<<4|1<<13))
	binary.LittleEndian.PutUint16(data[8:10], uint16(width))
	binary.LittleEndian.PutUint16(data[10:12], uint16(height))
	binary.LittleEndian.PutUint16(data[12:14], uint16(width*4))
	binary.LittleEndian.PutUint32(data[14:18], uint32(len(pixels)))
	copy(data[18:], pixels)
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
}

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

func TestParsePreservesDynamicTileNamesAndRoads(t *testing.T) {
	tiles := append([]byte("VRSN\x00\x01\x00\x00\x00"), []byte{6, 0, 7, 1, 6, 0, 7, 1}...)
	source := []byte(`return {map={width=2,height=2,tiles="` + base64.StdEncoding.EncodeToString(tiles) + `",world_tile_map={GRASS=6,MONKEY_DOCK=263},roads={{3,{-2,2},{0,0},{2,-2}},{1,{-1,-1},{1,1}}}},ents={}}`)
	parsed, err := Parse(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TileNames[263] != "MONKEY_DOCK" || len(parsed.Roads) != 2 || parsed.Roads[0].Kind != 3 || len(parsed.Roads[0].Points) != 3 {
		t.Fatalf("dynamic map data = names %#v, roads %#v", parsed.TileNames, parsed.Roads)
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
	renderer.AssetsPath = fixtureAssets(t)
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
	if err != nil || decoded.Bounds().Dx() != 16 || decoded.Bounds().Dy() != 16 {
		t.Fatalf("terrain bounds = %#v, %v", decoded.Bounds(), err)
	}
	if got := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA); got != (color.NRGBA{R: 30, G: 90, B: 130, A: 255}) {
		t.Fatalf("top-left tile = %#v", got)
	}
	if got := color.NRGBAModel.Convert(decoded.At(0, 15)).(color.NRGBA); got != (color.NRGBA{R: 40, G: 40, B: 40, A: 255}) {
		t.Fatalf("bottom-left tile = %#v", got)
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
