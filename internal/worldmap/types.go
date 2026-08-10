package worldmap

import "time"

type Layer string

const (
	LayerTerrain    Layer = "terrain"
	LayerIcons      Layer = "icons"
	LayerFeatures   Layer = "features"
	LayerWorldState Layer = "worldState"
)

type Session struct {
	ID          string    `json:"id"`
	RoomID      string    `json:"roomId"`
	WorldID     string    `json:"worldId"`
	SessionID   string    `json:"sessionId"`
	FileName    string    `json:"fileName"`
	Size        int64     `json:"size"`
	PlayerCount int       `json:"playerCount"`
	Latest      bool      `json:"latest"`
	ModifiedAt  time.Time `json:"modifiedAt"`
}

type GenerateRequest struct {
	WorldID   string  `json:"worldId"`
	SessionID string  `json:"sessionId"`
	Layers    []Layer `json:"layers"`
}

type Map struct {
	ID              string     `json:"id"`
	RoomID          string     `json:"roomId"`
	WorldID         string     `json:"worldId"`
	SessionID       string     `json:"sessionId"`
	SessionLabel    string     `json:"sessionLabel"`
	Status          string     `json:"status"`
	Stage           string     `json:"stage,omitempty"`
	Layers          []Layer    `json:"layers"`
	Width           int        `json:"width,omitempty"`
	Height          int        `json:"height,omitempty"`
	FeatureCount    int        `json:"featureCount,omitempty"`
	WarningCount    int        `json:"warningCount,omitempty"`
	SourceSHA256    string     `json:"sourceSha256,omitempty"`
	RendererVersion string     `json:"rendererVersion,omitempty"`
	Log             string     `json:"log,omitempty"`
	ErrorMessage    string     `json:"errorMessage,omitempty"`
	SourceJobID     string     `json:"sourceJobId"`
	CreatedAt       time.Time  `json:"createdAt"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
}

type Artifact string

const (
	ArtifactManifest Artifact = "manifest"
	ArtifactFeatures Artifact = "features"
)
