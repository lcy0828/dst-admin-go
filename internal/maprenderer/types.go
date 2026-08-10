package maprenderer

import "time"

const (
	ProtocolVersion  = "1"
	TerrainFileName  = "terrain.png"
	ManifestFileName = "manifest.json"
	FeaturesFileName = "features.json"
	IconsFileName    = "icons.png"
)

type Probe struct {
	ProtocolVersion string       `json:"protocolVersion"`
	RendererVersion string       `json:"rendererVersion"`
	Capabilities    Capabilities `json:"capabilities"`
}

type Capabilities struct {
	InputFormats []string `json:"inputFormats"`
	Artifacts    []string `json:"artifacts"`
	MaxInputSize int64    `json:"maxInputSize"`
}

type Manifest struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	RendererVersion string                 `json:"rendererVersion"`
	GeneratedAt     time.Time              `json:"generatedAt"`
	SourceSHA256    string                 `json:"sourceSha256"`
	Map             MapInfo                `json:"map"`
	Layers          []LayerDescriptor      `json:"layers"`
	Statistics      Statistics             `json:"statistics"`
	WorldState      map[string]interface{} `json:"worldState"`
	Warnings        []string               `json:"warnings"`
}

type MapInfo struct {
	TileWidth           int                 `json:"tileWidth"`
	TileHeight          int                 `json:"tileHeight"`
	ImageWidth          int                 `json:"imageWidth"`
	ImageHeight         int                 `json:"imageHeight"`
	PixelsPerTile       int                 `json:"pixelsPerTile"`
	WorldUnitsPerTile   int                 `json:"worldUnitsPerTile"`
	WorldBounds         Bounds              `json:"worldBounds"`
	CoordinateTransform CoordinateTransform `json:"coordinateTransform"`
}

type Bounds struct {
	MinX float64 `json:"minX"`
	MinZ float64 `json:"minZ"`
	MaxX float64 `json:"maxX"`
	MaxZ float64 `json:"maxZ"`
}

type CoordinateTransform struct {
	PixelX string `json:"pixelX"`
	PixelY string `json:"pixelY"`
}

type LayerDescriptor struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	File     string `json:"file"`
	MimeType string `json:"mimeType"`
}

type Statistics struct {
	TileCount        int            `json:"tileCount"`
	UnknownTileCount int            `json:"unknownTileCount"`
	FeatureCount     int            `json:"featureCount"`
	IconFeatureCount int            `json:"iconFeatureCount"`
	PrefabCounts     map[string]int `json:"prefabCounts"`
}

type FeatureCollection struct {
	ProtocolVersion string    `json:"protocolVersion"`
	Features        []Feature `json:"features"`
}

type Feature struct {
	ID         string                 `json:"id"`
	Prefab     string                 `json:"prefab"`
	Category   string                 `json:"category"`
	X          float64                `json:"x"`
	Z          float64                `json:"z"`
	PixelX     float64                `json:"pixelX"`
	PixelY     float64                `json:"pixelY"`
	Icon       *IconReference         `json:"icon,omitempty"`
	Properties map[string]interface{} `json:"properties"`
}

type IconReference struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Road struct {
	Kind   int
	Points []WorldPoint
}

type WorldPoint struct {
	X float64
	Z float64
}

type ParsedSave struct {
	TileWidth  int
	TileHeight int
	TileIDs    []uint16
	TileNames  map[uint16]string
	Roads      []Road
	Features   []Feature
	WorldState map[string]interface{}
	Warnings   []string
}
