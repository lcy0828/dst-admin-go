// Package moddiagnostics associates bounded, read-only Runtime observations.
// It never executes mod code or treats a stack frame as proof of causation.
package moddiagnostics

import "time"

const (
	MaxMods         = 512
	MaxEvidence     = 4
	MaxContextLines = 40
)

type Version struct {
	AppID       string `json:"appId,omitempty"`
	SteamBuild  string `json:"steamBuild,omitempty"`
	GameVersion string `json:"gameVersion,omitempty"`
}

type Mod struct {
	DSTCompatible *bool    `json:"dstCompatible,omitempty"`
	APIVersion    string   `json:"apiVersion,omitempty"`
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Version       string   `json:"version,omitempty"`
	Manifest      string   `json:"manifest,omitempty"`
	FileStatus    string   `json:"fileStatus,omitempty"`
	Dependencies  []string `json:"dependencies,omitempty"`
}

// Observation contains identifiers/hashes only, never configuration contents,
// player records, save contents, tokens or remote credentials.
type Observation struct {
	RoomName              string    `json:"roomName,omitempty"`
	RoomID                string    `json:"roomId"`
	WorldID               string    `json:"worldId"`
	WorldName             string    `json:"worldName,omitempty"`
	TargetID              string    `json:"targetId,omitempty"`
	InstallationID        string    `json:"installationId,omitempty"`
	TopologyRevision      string    `json:"topologyRevision,omitempty"`
	RuntimeState          string    `json:"runtimeState,omitempty"`
	RuntimeMode           string    `json:"runtimeMode,omitempty"`
	OS                    string    `json:"os,omitempty"`
	Version               Version   `json:"version"`
	ConfigurationRevision string    `json:"configurationRevision,omitempty"`
	Mods                  []Mod     `json:"mods"`
	ObservedAt            time.Time `json:"observedAt"`
	Limitations           []string  `json:"limitations"`
}

type Evidence struct {
	Code        string   `json:"code"`
	FirstCursor int64    `json:"firstCursor"`
	ErrorCursor int64    `json:"errorCursor"`
	Lines       []string `json:"lines"`
	ModIDs      []string `json:"modIds"`
	Truncated   bool     `json:"truncated"`
}

type Finding struct {
	Code   string   `json:"code"`
	Kind   string   `json:"kind"` // observed or suspected, never an invented confidence score
	ModIDs []string `json:"modIds,omitempty"`
}

type Change struct {
	Field  string `json:"field"`
	ModID  string `json:"modId,omitempty"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

type Baseline struct {
	ReleaseID   string      `json:"releaseId"`
	CapturedAt  time.Time   `json:"capturedAt"`
	Observation Observation `json:"observation"`
}

type Report struct {
	Status          string      `json:"status"` // observed_problem, suspected, no_known_issue, insufficient_evidence
	Observation     Observation `json:"observation"`
	Baseline        *Baseline   `json:"baseline,omitempty"`
	Changes         []Change    `json:"changes"`
	Findings        []Finding   `json:"findings"`
	Evidence        []Evidence  `json:"evidence"`
	LogStartedAt    *time.Time  `json:"logStartedAt,omitempty"`
	IncidentAt      *time.Time  `json:"incidentAt,omitempty"`
	LogCorrelation  string      `json:"logCorrelation"` // matched, historical, unknown, not_requested
	Limitations     []string    `json:"limitations"`
	Recommendations []string    `json:"recommendations"`
}

// Scope is taken from the authoritative update plan; advisories never change
// the execution plan's readiness/hash or initiate any maintenance operation.
type Scope struct {
	RoomID, WorldID, TargetID, InstallationID, TopologyRevision, RuntimeMode, OS string
	Version                                                                      Version
}
