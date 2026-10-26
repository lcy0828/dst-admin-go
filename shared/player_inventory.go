package shared

import "time"

const PlayerInventoryCapability = "runtime.player-inventory.read.v1"
const RuntimeActionPlayerInventory RuntimeAction = "runtime.player-inventory.read"

type PlayerInventoryRequest struct {
	PlayerID string `json:"playerId"`
	Source   string `json:"source"`
	SourceID string `json:"sourceId,omitempty"`
	Refresh  bool   `json:"refresh,omitempty"`
}

type InventoryMetric struct {
	Value        *float64 `json:"value"`
	Maximum      *float64 `json:"maximum"`
	Ratio        *float64 `json:"ratio"`
	Unit         string   `json:"unit"`
	Availability string   `json:"availability"`
	Reason       string   `json:"reason,omitempty"`
}

type InventoryItem struct {
	NodeID        string                     `json:"nodeId"`
	Slot          string                     `json:"slot"`
	Prefab        string                     `json:"prefab"`
	Name          string                     `json:"name"`
	NameSource    string                     `json:"nameSource"`
	Count         *float64                   `json:"count"`
	RecordSource  string                     `json:"recordSource"`
	Metrics       map[string]InventoryMetric `json:"metrics,omitempty"`
	Contents      []InventoryItem            `json:"contents"`
	ContainerKind string                     `json:"containerKind,omitempty"`
	SlotCount     *int                       `json:"slotCount"`
	Warnings      []string                   `json:"warnings,omitempty"`
}

type InventoryCandidate struct {
	SourceID       string     `json:"sourceId"`
	WorldID        string     `json:"worldId"`
	WorldName      string     `json:"worldName"`
	SessionID      string     `json:"sessionId"`
	FileModifiedAt *time.Time `json:"fileModifiedAt"`
}

type PlayerInventory struct {
	SchemaVersion  int                  `json:"schemaVersion"`
	PlayerID       string               `json:"playerId"`
	RoomID         string               `json:"roomId"`
	WorldID        string               `json:"worldId"`
	WorldName      string               `json:"worldName"`
	Source         string               `json:"source"`
	SourceID       string               `json:"sourceId,omitempty"`
	SessionID      string               `json:"sessionId"`
	Character      string               `json:"character"`
	CapturedAt     time.Time            `json:"capturedAt"`
	SavedAt        *time.Time           `json:"savedAt"`
	FileModifiedAt *time.Time           `json:"fileModifiedAt"`
	TimeSource     string               `json:"timeSource"`
	CacheAgeMS     int64                `json:"cacheAgeMs"`
	Status         string               `json:"status"`
	Coverage       string               `json:"coverage"`
	Empty          bool                 `json:"empty"`
	SlotCount      *int                 `json:"slotCount"`
	Equipment      []InventoryItem      `json:"equipment"`
	Inventory      []InventoryItem      `json:"inventory"`
	ActiveItem     *InventoryItem       `json:"activeItem"`
	Warnings       []string             `json:"warnings"`
	LimitsReached  []string             `json:"limitsReached"`
	Code           string               `json:"code,omitempty"`
	Candidates     []InventoryCandidate `json:"candidates,omitempty"`
}

type InventoryError struct {
	Code       string               `json:"code"`
	Candidates []InventoryCandidate `json:"candidates,omitempty"`
}

func (e *InventoryError) Error() string { return e.Code }
