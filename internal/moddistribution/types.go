package moddistribution

import (
	"errors"
	"time"
)

const (
	ManifestVersion = 1
	JournalVersion  = 1

	maxManifestEntries = 100000
	maxManifestBytes   = int64(64 << 30)
	maxSingleFileBytes = int64(16 << 30)
	maxOverridesBytes  = 8 << 20
	maxPlanConfigBytes = 32 << 20
)

var (
	ErrInvalidInput        = errors.New("mod distribution input is invalid")
	ErrNotFound            = errors.New("mod content is not cached")
	ErrConflict            = errors.New("mod distribution state conflicts with the requested release")
	ErrIntegrity           = errors.New("mod content integrity verification failed")
	ErrUnsafePath          = errors.New("mod distribution path is outside a trusted root or contains a symbolic link")
	ErrInsufficientSpace   = errors.New("insufficient disk space for mod distribution")
	ErrOperationInProgress = errors.New("another mod distribution operation is in progress")
)

type Metadata struct {
	Title             string    `json:"title,omitempty"`
	Version           string    `json:"version,omitempty"`
	PublishedFileSize int64     `json:"publishedFileSize,omitempty"`
	SteamUpdatedAt    time.Time `json:"steamUpdatedAt,omitempty"`
}

type Entry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

type Manifest struct {
	Version        int       `json:"version"`
	WorkshopID     string    `json:"workshopId"`
	TreeSHA256     string    `json:"treeSha256"`
	ManifestSHA256 string    `json:"manifestSha256"`
	Size           int64     `json:"size"`
	FileCount      int       `json:"fileCount"`
	Entries        []Entry   `json:"entries"`
	Metadata       Metadata  `json:"metadata"`
	CreatedAt      time.Time `json:"createdAt"`
}

type TrustedInstallation struct {
	ID                  string `json:"id"`
	NodeID              string `json:"nodeId"`
	ServerPath          string `json:"serverPath"`
	SavePath            string `json:"savePath"`
	WorkshopContentPath string `json:"workshopContentPath,omitempty"`
}

type Config struct {
	CacheRoot     string                `json:"cacheRoot"`
	StateRoot     string                `json:"stateRoot"`
	NodeID        string                `json:"nodeId"`
	ReserveBytes  int64                 `json:"reserveBytes"`
	Installations []TrustedInstallation `json:"installations"`
}

type ModVersion struct {
	WorkshopID string `json:"workshopId"`
	TreeSHA256 string `json:"treeSha256"`
}

// ShardRelease is one world's complete desired Mod state. ModOverrides is
// intentionally kept as Lua bytes so configurations unsupported by a typed UI
// can still be published without losing information.
type ShardRelease struct {
	InstallationID string       `json:"installationId"`
	RoomID         string       `json:"roomId"`
	RoomDirectory  string       `json:"roomDirectory"`
	WorldID        string       `json:"worldId"`
	WorldDirectory string       `json:"worldDirectory"`
	Mods           []ModVersion `json:"mods"`
	ModOverrides   []byte       `json:"modOverrides"`
}

// PlanInput is the complete desired shard state for every included
// installation. Omitting a Mod removes it from the managed setup union, while
// cached files are retained for reuse and rollback.
type PlanInput struct {
	OperationID string         `json:"operationId"`
	NodeID      string         `json:"nodeId"`
	Shards      []ShardRelease `json:"shards"`
}

type InstallationPlan struct {
	InstallationID  string         `json:"installationId"`
	NodeID          string         `json:"nodeId"`
	Mods            []ModVersion   `json:"mods"`
	Shards          []ShardRelease `json:"shards"`
	ManagedSetup    []byte         `json:"managedSetup"`
	SetupBaseSHA256 string         `json:"setupBaseSha256,omitempty"`
}

type Plan struct {
	OperationID   string             `json:"operationId"`
	NodeID        string             `json:"nodeId"`
	CreatedAt     time.Time          `json:"createdAt"`
	Installations []InstallationPlan `json:"installations"`
}

type Phase string

const (
	PhasePreparing   Phase = "preparing"
	PhasePrepared    Phase = "prepared"
	PhasePublishing  Phase = "publishing"
	PhasePublished   Phase = "published"
	PhaseCompleting  Phase = "completing"
	PhaseCommitted   Phase = "committed"
	PhaseRollingBack Phase = "rolling_back"
)

type MutationKind string

const (
	MutationMod       MutationKind = "mod"
	MutationOverrides MutationKind = "modoverrides"
	MutationSetup     MutationKind = "dedicated_server_mods_setup"
)

type Mutation struct {
	Index          int          `json:"index"`
	Kind           MutationKind `json:"kind"`
	InstallationID string       `json:"installationId"`
	WorkshopID     string       `json:"workshopId,omitempty"`
	TreeSHA256     string       `json:"treeSha256,omitempty"`
	RoomDirectory  string       `json:"roomDirectory,omitempty"`
	WorldDirectory string       `json:"worldDirectory,omitempty"`
	ConfigSHA256   string       `json:"configSha256,omitempty"`
	OriginalSHA256 string       `json:"originalSha256,omitempty"`
	HadOriginal    bool         `json:"hadOriginal"`
}

type Journal struct {
	Version         int        `json:"version"`
	OperationID     string     `json:"operationId"`
	Phase           Phase      `json:"phase"`
	NextMutation    int        `json:"nextMutation"`
	Plan            Plan       `json:"plan"`
	Mutations       []Mutation `json:"mutations"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	IntegritySHA256 string     `json:"integritySha256"`
}

type InstallationState struct {
	InstallationID     string                `json:"installationId"`
	LastOperationID    string                `json:"lastOperationId"`
	Mods               map[string]string     `json:"mods"`
	ManagedSetupSHA256 string                `json:"managedSetupSha256"`
	Shards             map[string]ShardState `json:"shards"`
	UpdatedAt          time.Time             `json:"updatedAt"`
}

type ShardState struct {
	RoomID         string       `json:"roomId"`
	RoomDirectory  string       `json:"roomDirectory"`
	WorldID        string       `json:"worldId"`
	WorldDirectory string       `json:"worldDirectory"`
	Mods           []ModVersion `json:"mods"`
	ConfigSHA256   string       `json:"configSha256"`
}

type RecoveryResult struct {
	OperationID string `json:"operationId"`
	Action      string `json:"action"`
	Error       string `json:"error,omitempty"`
}
