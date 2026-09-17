package gameupdate

import "time"

type VersionReport struct {
	Installed           bool             `json:"installed"`
	AppID               string           `json:"appId"`
	LocalVersion        string           `json:"localVersion,omitempty"`
	GameVersion         string           `json:"gameVersion,omitempty"`
	Branch              string           `json:"branch,omitempty"`
	LatestVersion       string           `json:"latestVersion,omitempty"`
	UpToDate            *bool            `json:"upToDate,omitempty"`
	InstallPath         string           `json:"installPath"`
	UpdateMethod        string           `json:"updateMethod"`
	UpdateSupported     bool             `json:"updateSupported"`
	UpdateBlockedReason string           `json:"updateBlockedReason,omitempty"`
	SteamCMDAvailable   bool             `json:"steamcmdAvailable"`
	SteamCMDPath        string           `json:"steamcmdPath,omitempty"`
	CheckError          string           `json:"checkError,omitempty"`
	OfficialRelease     *OfficialRelease `json:"officialRelease,omitempty"`
	OfficialCheckError  string           `json:"officialCheckError,omitempty"`
	CheckedAt           time.Time        `json:"checkedAt"`
}

type OfficialRelease struct {
	Version     string           `json:"version"`
	ReleaseID   string           `json:"releaseId"`
	PublishedAt time.Time        `json:"publishedAt"`
	URL         string           `json:"url"`
	Source      string           `json:"source"`
	CheckedAt   time.Time        `json:"checkedAt"`
	Stale       bool             `json:"stale"`
	TestRelease *OfficialRelease `json:"testRelease,omitempty"`
}

type UpdateRequest struct {
	Confirmation   string `json:"confirmation"`
	RestartRunning bool   `json:"restartRunning"`
	CleanCache     bool   `json:"cleanCache"`
}

type Run struct {
	JobID         string     `json:"jobId"`
	Status        string     `json:"status"`
	BeforeVersion string     `json:"beforeVersion,omitempty"`
	AfterVersion  string     `json:"afterVersion,omitempty"`
	CleanCache    bool       `json:"cleanCache"`
	Log           string     `json:"log"`
	ErrorMessage  string     `json:"errorMessage,omitempty"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}
