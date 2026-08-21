package modpublication

import (
	"context"
	"time"
)

type Catalog interface {
	ManagedWorlds(context.Context) ([]ManagedWorld, error)
}

type Placement interface {
	AppliedPlacements(context.Context) (PlacementSnapshot, error)
}

type ContentSource interface {
	Resolve(context.Context, ModRequirement) (ContentArtifact, error)
}

type Runtime interface {
	Observe(context.Context, AppliedPlacement) (RuntimeObservation, error)
	EnsureCache(context.Context, TargetPlan, RuntimeOperation) error
	Prepare(context.Context, TargetPlan, RuntimeOperation) error
	Publish(context.Context, TargetPlan, RuntimeOperation) error
	Rollback(context.Context, TargetPlan, RuntimeOperation) error
	Complete(context.Context, TargetPlan, RuntimeOperation) error
}

type ActivationRuntime interface {
	Status(context.Context, WorldPlan) (ShardRuntimeObservation, error)
	CaptureLogCursor(context.Context, WorldPlan) (LogCursor, error)
	Stop(context.Context, WorldPlan, RuntimeOperation) error
	Start(context.Context, WorldPlan, RuntimeOperation) error
	ReadLogs(context.Context, WorldPlan, LogCursor) (ShardLogObservation, error)
}

type LifecycleNotifier interface {
	BeforeOperations(context.Context, []string, string, string, string) error
}

type Lease interface {
	Acquire(context.Context, string, string, time.Duration) (Fence, error)
	Renew(context.Context, Fence, time.Duration) (Fence, error)
	Release(Fence) error
}

type ProtectionRequest struct {
	PublicationID    string   `json:"publicationId"`
	RoomIDs          []string `json:"roomIds"`
	TopologyRevision string   `json:"topologyRevision"`
	PlanHash         string   `json:"planHash"`
	Fences           []Fence  `json:"fences"`
}

type ProtectionBackup struct {
	IDs []string `json:"ids"`
}

type Backup interface {
	CreateProtection(context.Context, ProtectionRequest) (ProtectionBackup, error)
}
