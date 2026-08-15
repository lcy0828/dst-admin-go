package runtimedriver

import (
	"context"
	"errors"
	"time"

	"dont/shared"
)

var (
	ErrInvalidTarget      = errors.New("runtime driver target is invalid")
	ErrCapabilityMissing  = errors.New("runtime driver capability is unavailable")
	ErrUnsupportedRuntime = errors.New("runtime driver kind is unsupported")
)

type Kind string

const (
	KindNative     Kind = "native"
	KindContainer  Kind = "container"
	KindKubernetes Kind = "kubernetes"
)

type Capability string

const (
	CapabilityLifecycle       Capability = "lifecycle"
	CapabilityConsoleInput    Capability = "consoleInput"
	CapabilityConsoleHealth   Capability = "consoleHealth"
	CapabilityRawConsole      Capability = "rawConsole"
	CapabilityOperationProof  Capability = "operationProof"
	CapabilityLogContinuation Capability = "logContinuation"
	CapabilityArtifacts       Capability = "artifacts"
	CapabilitySnapshotBarrier Capability = "snapshotBarrier"
	CapabilityBackupStage     Capability = "backupStage"
	CapabilityBackupRestore   Capability = "backupRestore"
	CapabilityModPrepare      Capability = "modPrepare"
	CapabilityModPublish      Capability = "modPublish"
	CapabilityExclusiveCPU    Capability = "exclusiveCpu"
	CapabilityPublishedUDP    Capability = "publishedUdpEndpoint"
)

type Target struct {
	TargetID         string
	InstallationID   string
	RoomID           string
	WorldID          string
	Cluster          string
	Shard            string
	TopologyRevision string
}

type Operation struct {
	ID             string
	Key            string
	LeaseID        string
	FencingToken   uint64
	LeaseExpiresAt *time.Time
}

type MigrationDescriptor struct {
	MigrationID string
	Size        int64
	SHA256      string
	RecoveryRef string
}

type MigrationChunk struct {
	Offset     int64
	NextOffset int64
	Size       int64
	SHA256     string
	Data       []byte
	Complete   bool
}

type Driver interface {
	Kind() Kind
	Capabilities() []Capability
	Status(context.Context, Target) (shared.ShardRuntimeStatus, error)
	ExecuteShard(context.Context, Target, Operation, shared.ShardAction, time.Duration) (shared.ShardOperationResult, error)
	SendConsole(context.Context, Target, Operation, shared.RuntimeConsoleRequest, time.Duration) (shared.RuntimeOperationResult, error)
	ConsoleHealth(context.Context, Target) (shared.RuntimeConsoleHealth, error)
	ObserveOperation(context.Context, Target, string, string) (shared.RuntimeOperationEvidence, error)
	ReadLogs(context.Context, Target, shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error)
	ReadArtifacts(context.Context, Target, shared.ArtifactKind) (shared.RuntimeArtifactBundle, error)
	PrepareMigrationExport(context.Context, Target, Operation, string) (MigrationDescriptor, error)
	ReadMigrationExport(context.Context, Target, string, int64) (MigrationChunk, error)
	ReleaseMigrationExport(context.Context, Target, Operation, string) error
	BeginMigrationImport(context.Context, Target, Operation, MigrationDescriptor) error
	WriteMigrationImport(context.Context, Target, Operation, MigrationDescriptor, int64, []byte) (int64, error)
	CommitMigrationImport(context.Context, Target, Operation, string) error
	RollbackMigrationTarget(context.Context, Target, Operation, string) error
	CompleteMigrationTarget(context.Context, Target, Operation, string) error
	FinalizeMigrationSource(context.Context, Target, Operation, string) (string, error)
	RollbackMigrationSource(context.Context, Target, Operation, string) error
	CompleteMigrationSource(context.Context, Target, Operation, string) (string, error)
}

func HasCapability(driver Driver, expected Capability) bool {
	if driver == nil {
		return false
	}
	for _, capability := range driver.Capabilities() {
		if capability == expected {
			return true
		}
	}
	return false
}
