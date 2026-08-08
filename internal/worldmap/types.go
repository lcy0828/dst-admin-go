package worldmap

import "time"

type Layer string

const (
	LayerTerrain     Layer = "terrain"
	LayerWalrusCamps Layer = "walrusCamps"
	LayerSpawnPoints Layer = "spawnPoints"
	LayerPlayers     Layer = "players"
	LayerWorldState  Layer = "worldState"
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
	ID           string     `json:"id"`
	RoomID       string     `json:"roomId"`
	WorldID      string     `json:"worldId"`
	SessionID    string     `json:"sessionId"`
	SessionLabel string     `json:"sessionLabel"`
	Status       string     `json:"status"`
	Stage        string     `json:"stage,omitempty"`
	Layers       []Layer    `json:"layers"`
	Width        int        `json:"width,omitempty"`
	Height       int        `json:"height,omitempty"`
	Log          string     `json:"log,omitempty"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
	SourceJobID  string     `json:"sourceJobId"`
	CreatedAt    time.Time  `json:"createdAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
}
