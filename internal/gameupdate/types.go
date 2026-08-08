package gameupdate

import "time"

type VersionReport struct {
	Installed         bool      `json:"installed"`
	LocalVersion      string    `json:"localVersion,omitempty"`
	LatestVersion     string    `json:"latestVersion,omitempty"`
	UpToDate          *bool     `json:"upToDate,omitempty"`
	InstallPath       string    `json:"installPath"`
	SteamCMDAvailable bool      `json:"steamcmdAvailable"`
	SteamCMDPath      string    `json:"steamcmdPath,omitempty"`
	CheckError        string    `json:"checkError,omitempty"`
	CheckedAt         time.Time `json:"checkedAt"`
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
