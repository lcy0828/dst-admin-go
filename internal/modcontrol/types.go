package modcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"
)

var (
	ErrInvalidRequest   = errors.New("mod publication request is invalid")
	ErrConfirmation     = errors.New("mod publication plan hash confirmation is required")
	ErrTopologyChanged  = errors.New("mod publication topology revision changed")
	ErrPublicationState = errors.New("mod publication cannot be retried from its current state")
)

type Request struct {
	SourceWorldID                  string                          `json:"sourceWorldId,omitempty"`
	Action                         string                          `json:"action"`
	ModID                          string                          `json:"modId,omitempty"`
	ModIDs                         []string                        `json:"modIds,omitempty"`
	WorldIDs                       []string                        `json:"worldIds,omitempty"`
	Enabled                        bool                            `json:"enabled"`
	PreserveEnabled                bool                            `json:"preserveEnabled,omitempty"`
	IncludeDependencies            bool                            `json:"includeDependencies"`
	ExpectedConfigurationRevision  string                          `json:"expectedConfigurationRevision,omitempty"`
	ExpectedConfigurationRevisions map[string]string               `json:"expectedConfigurationRevisions,omitempty"`
	ExpectedTopologyRevision       string                          `json:"expectedTopologyRevision,omitempty"`
	Patch                          map[string]json.RawMessage      `json:"patch,omitempty"`
	PlanHash                       string                          `json:"planHash,omitempty"`
	Confirmation                   string                          `json:"confirmation,omitempty"`
	Activation                     modpublication.ActivationPolicy `json:"activation,omitempty"`
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
	ResolveCachedRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error)
	ResolveCachedExecution(context.Context, string, string) (topology.ExecutionPlacement, error)
}

type ModCatalog interface {
	ResolveDependencies(context.Context, string, bool) ([]string, error)
	Describe(context.Context, []string) (map[string]mods.SteamMod, error)
	ConfigurationFromContent(context.Context, string, string, string, []byte) (mods.ModConfiguration, error)
	List(context.Context, string) (mods.ModList, error)
	ListFromOverrides(context.Context, string, map[string][]byte) (mods.ModList, error)
}

type PublicationCoordinator interface {
	Preview(context.Context, string) (modpublication.Plan, error)
	Publish(context.Context, modpublication.PublishRequest) (modpublication.Publication, error)
	BeginTransaction(context.Context, modpublication.PublishRequest) (modpublication.PreparedTransaction, error)
	Get(string) (modpublication.Publication, error)
	List(string, int, int) ([]modpublication.Publication, int, error)
	Recover(context.Context) ([]modpublication.Publication, error)
	RecoverOne(context.Context, string, ...string) (modpublication.Publication, error)
	Activate(context.Context, string, string, modpublication.ActivationPolicy) (modpublication.Publication, error)
}

type Clock func() time.Time

type PlanConvergence interface {
	Converged(context.Context, modpublication.Plan) (bool, error)
}

type RuntimeFileWorld struct {
	RoomID         string
	RoomDirectory  string
	WorldID        string
	WorldDirectory string
}

type RuntimeFileRequest struct {
	TargetID         string
	InstallationID   string
	TopologyRevision string
	WorkshopIDs      []string
	Worlds           []RuntimeFileWorld
}

type RuntimeFileObserver interface {
	ObserveRuntimeModFiles(context.Context, RuntimeFileRequest) (*shared.RuntimeModFilesObservation, error)
	ObserveRuntimeModInventory(context.Context, string, string) (*shared.RuntimeModFilesObservation, error)
}

type CachePreparer interface {
	EnsureCache(context.Context, modpublication.Plan) (modpublication.CachePreparation, error)
}

type InstallationContentFetcher interface {
	UpdateInstallationMods(context.Context, string, string, []string, io.Writer) (mods.ActionResult, error)
	LinkInstallationMods(context.Context, string, string, []string) error
}

type ModConfigurationPublisher interface {
	PublishModOverrides(context.Context, string, []runtimedriver.ModOverridesUpdate) (int, error)
}

type ReplicaReader interface {
	Room(string) (modpublication.RoomReplicaState, error)
}

type PlacementMigrationReconciler interface {
	PreparePlacementMigration(context.Context, topology.MigrationPlacement, operationlease.Lease) (modpublication.PreparedTransaction, error)
}
