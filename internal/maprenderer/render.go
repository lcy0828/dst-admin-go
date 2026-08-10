package maprenderer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	maxJSONArtifactSize = int64(64 * 1024 * 1024)
)

type Renderer struct {
	Version    string
	AssetsPath string
	Now        func() time.Time
}

func New(version string) *Renderer {
	if version == "" {
		version = "dev"
	}
	return &Renderer{Version: version, Now: time.Now}
}

func (r *Renderer) Probe() Probe {
	return Probe{
		ProtocolVersion: ProtocolVersion, RendererVersion: r.Version,
		Capabilities: Capabilities{
			InputFormats: []string{"lua", "klei-text-v1", "klei-deflate-v1"},
			Artifacts:    []string{TerrainFileName, ManifestFileName, FeaturesFileName},
			MaxInputSize: MaxInputSize,
		},
	}
}

func (r *Renderer) Render(ctx context.Context, inputPath, outputDirectory string) (Manifest, error) {
	assets, err := DiscoverAssets(r.AssetsPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("load official DST assets: %w", err)
	}
	definitions, err := LoadTileDefinitions(ctx, assets)
	if err != nil {
		return Manifest{}, fmt.Errorf("load official DST tile definitions: %w", err)
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("open input snapshot: %w", err)
	}
	hash := sha256.New()
	source, decodeErr := DecodeSave(io.TeeReader(input, hash))
	closeErr := input.Close()
	if decodeErr != nil {
		return Manifest{}, decodeErr
	}
	if closeErr != nil {
		return Manifest{}, fmt.Errorf("close input snapshot: %w", closeErr)
	}
	parsed, err := Parse(ctx, source)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateOutputDirectory(outputDirectory); err != nil {
		return Manifest{}, err
	}
	terrainPath := filepath.Join(outputDirectory, TerrainFileName)
	terrain, err := os.OpenFile(terrainPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return Manifest{}, fmt.Errorf("create terrain output: %w", err)
	}
	unknownTiles, terrainErr := RenderTerrain(terrain, parsed, assets, definitions)
	terrainCloseErr := terrain.Close()
	if terrainErr != nil {
		return Manifest{}, terrainErr
	}
	if terrainCloseErr != nil {
		return Manifest{}, fmt.Errorf("close terrain output: %w", terrainCloseErr)
	}
	if unknownTiles > 0 {
		parsed.Warnings = appendWarning(parsed.Warnings, fmt.Sprintf("%d 个地形块没有可用的官方小地图纹理，已使用稳定备用颜色显示", unknownTiles))
	}
	prefabCounts := make(map[string]int)
	for _, feature := range parsed.Features {
		prefabCounts[feature.Prefab]++
	}
	scale := pixelsPerTile(parsed.TileWidth, parsed.TileHeight)
	manifest := Manifest{
		ProtocolVersion: ProtocolVersion, RendererVersion: r.Version, GeneratedAt: r.Now().UTC(),
		SourceSHA256: hex.EncodeToString(hash.Sum(nil)),
		Map: MapInfo{
			TileWidth: parsed.TileWidth, TileHeight: parsed.TileHeight,
			ImageWidth: parsed.TileWidth * scale, ImageHeight: parsed.TileHeight * scale,
			PixelsPerTile: scale, WorldUnitsPerTile: worldUnitsPerTile,
			WorldBounds: Bounds{
				MinX: -float64(parsed.TileWidth*worldUnitsPerTile) / 2,
				MinZ: -float64(parsed.TileHeight*worldUnitsPerTile) / 2,
				MaxX: float64(parsed.TileWidth*worldUnitsPerTile) / 2,
				MaxZ: float64(parsed.TileHeight*worldUnitsPerTile) / 2,
			},
			CoordinateTransform: CoordinateTransform{
				PixelX: "(x - minX) / worldUnitsPerTile * pixelsPerTile",
				PixelY: "(maxZ - z) / worldUnitsPerTile * pixelsPerTile",
			},
		},
		Layers: []LayerDescriptor{
			{ID: "terrain", Kind: "raster", File: TerrainFileName, MimeType: "image/png"},
			{ID: "features", Kind: "vector", File: FeaturesFileName, MimeType: "application/json"},
		},
		Statistics: Statistics{
			TileCount: len(parsed.TileIDs), UnknownTileCount: unknownTiles,
			FeatureCount: len(parsed.Features), PrefabCounts: prefabCounts,
		},
		WorldState: parsed.WorldState, Warnings: parsed.Warnings,
	}
	sort.Strings(manifest.Warnings)
	features := FeatureCollection{ProtocolVersion: ProtocolVersion, Features: parsed.Features}
	if err := writeJSONExclusive(filepath.Join(outputDirectory, FeaturesFileName), features); err != nil {
		return Manifest{}, err
	}
	if err := writeJSONExclusive(filepath.Join(outputDirectory, ManifestFileName), manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func validateOutputDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("output path must be a real directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read output directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("output directory must be empty")
	}
	return nil
}

func writeJSONExclusive(path string, value interface{}) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	limited := &limitWriter{writer: file, remaining: maxJSONArtifactSize}
	encoder := json.NewEncoder(limited)
	encoder.SetEscapeHTML(false)
	encodeErr := encoder.Encode(value)
	closeErr := file.Close()
	if encodeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("encode %s: %w", filepath.Base(path), encodeErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %s: %w", filepath.Base(path), closeErr)
	}
	return nil
}

type limitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > w.remaining {
		return 0, fmt.Errorf("JSON artifact exceeds %s", humanBytes(maxJSONArtifactSize))
	}
	written, err := w.writer.Write(value)
	w.remaining -= int64(written)
	return written, err
}
