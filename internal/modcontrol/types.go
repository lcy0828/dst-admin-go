package modcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/topology"
)

var (
	ErrInvalidRequest   = errors.New("mod publication request is invalid")
	ErrConfirmation     = errors.New("mod publication plan hash confirmation is required")
	ErrTopologyChanged  = errors.New("mod publication topology revision changed")
	ErrPublicationState = errors.New("mod publication cannot be retried from its current state")
)

type Request struct {
	Action                        string                     `json:"action"`
	ModID                         string                     `json:"modId,omitempty"`
	ModIDs                        []string                   `json:"modIds,omitempty"`
	WorldIDs                      []string                   `json:"worldIds,omitempty"`
	Enabled                       bool                       `json:"enabled"`
	IncludeDependencies           bool                       `json:"includeDependencies"`
	ExpectedConfigurationRevision string                     `json:"expectedConfigurationRevision,omitempty"`
	ExpectedTopologyRevision      string                     `json:"expectedTopologyRevision,omitempty"`
	Patch                         map[string]json.RawMessage `json:"patch,omitempty"`
	PlanHash                      string                     `json:"planHash,omitempty"`
	Confirmation                  string                     `json:"confirmation,omitempty"`
}

type ListResult struct {
	Items  []modpublication.Publication `json:"items"`
	Total  int                          `json:"total"`
	Limit  int                          `json:"limit"`
	Offset int                          `json:"offset"`
}

type RoomCatalog interface {
	List() ([]rooms.Room, error)
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
	World(string, string) (rooms.World, error)
}

type PlacementResolver interface {
	ResolveRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error)
}

type ModCatalog interface {
	ResolveDependencies(context.Context, string, bool) ([]string, error)
	ConfigurationFromContent(context.Context, string, string, string, []byte) (mods.ModConfiguration, error)
	ListFromOverrides(context.Context, string, map[string][]byte) (mods.ModList, error)
}

type PublicationCoordinator interface {
	Preview(context.Context, string) (modpublication.Plan, error)
	Publish(context.Context, modpublication.PublishRequest) (modpublication.Publication, error)
	Get(string) (modpublication.Publication, error)
	List(string, int, int) ([]modpublication.Publication, int, error)
	Recover(context.Context) ([]modpublication.Publication, error)
	RecoverOne(context.Context, string) (modpublication.Publication, error)
}

type Clock func() time.Time
