package worldmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"time"

	"dont/internal/maprenderer"
)

// MemoryRenderer is a deterministic renderer wired only by the router in test mode.
type MemoryRenderer struct{}

func NewMemoryRenderer() *MemoryRenderer { return &MemoryRenderer{} }

func (*MemoryRenderer) Available() (bool, string) { return true, "memory" }

func (*MemoryRenderer) Info() RendererInfo {
	return RendererInfo{
		Available: true, Path: "memory", ProtocolVersion: maprenderer.ProtocolVersion, Version: "test",
		Artifacts: []string{maprenderer.TerrainFileName, maprenderer.ManifestFileName, maprenderer.FeaturesFileName},
	}
}

func (*MemoryRenderer) Render(ctx context.Context, input, output string, layers []Layer, log io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	inputFile, err := os.Open(input)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, hashErr := io.Copy(hash, inputFile)
	closeErr := inputFile.Close()
	if hashErr != nil {
		return hashErr
	}
	if closeErr != nil {
		return closeErr
	}
	sourceSHA256 := hex.EncodeToString(hash.Sum(nil))
	canvas := image.NewRGBA(image.Rect(0, 0, 960, 640))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{R: 29, G: 74, B: 48, A: 255}}, image.Point{}, draw.Src)
	for y := 0; y < 640; y += 80 {
		shade := color.RGBA{R: uint8(48 + y/20), G: uint8(105 + y/24), B: 61, A: 255}
		draw.Draw(canvas, image.Rect(80, y, 880, y+48), &image.Uniform{C: shade}, image.Point{}, draw.Src)
	}
	file, err := os.OpenFile(filepath.Join(output, maprenderer.TerrainFileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(file, canvas)
	closeErr = file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	if closeErr != nil {
		return closeErr
	}
	features := maprenderer.FeatureCollection{ProtocolVersion: maprenderer.ProtocolVersion, Features: []maprenderer.Feature{}}
	if err := writeMemoryJSON(filepath.Join(output, maprenderer.FeaturesFileName), features); err != nil {
		return err
	}
	manifest := maprenderer.Manifest{
		ProtocolVersion: maprenderer.ProtocolVersion, RendererVersion: "test", GeneratedAt: time.Now().UTC(),
		SourceSHA256: sourceSHA256,
		Map: maprenderer.MapInfo{
			TileWidth: 240, TileHeight: 160, ImageWidth: 960, ImageHeight: 640, PixelsPerTile: 4, WorldUnitsPerTile: 4,
			WorldBounds: maprenderer.Bounds{MinX: -480, MinZ: -320, MaxX: 480, MaxZ: 320},
			CoordinateTransform: maprenderer.CoordinateTransform{
				PixelX: "(x - minX) / worldUnitsPerTile * pixelsPerTile",
				PixelY: "(maxZ - z) / worldUnitsPerTile * pixelsPerTile",
			},
		},
		Layers: []maprenderer.LayerDescriptor{
			{ID: "terrain", Kind: "raster", File: maprenderer.TerrainFileName, MimeType: "image/png"},
			{ID: "features", Kind: "vector", File: maprenderer.FeaturesFileName, MimeType: "application/json"},
		},
		Statistics: maprenderer.Statistics{TileCount: 240 * 160, PrefabCounts: map[string]int{}},
		WorldState: map[string]interface{}{}, Warnings: []string{},
	}
	if err := writeMemoryJSON(filepath.Join(output, maprenderer.ManifestFileName), manifest); err != nil {
		return err
	}
	_, err = fmt.Fprintf(log, "[DST-ADMIN-TEST] rendered %d map layers\n", len(layers))
	return err
}

func writeMemoryJSON(path string, value interface{}) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	encodeErr := json.NewEncoder(file).Encode(value)
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}
