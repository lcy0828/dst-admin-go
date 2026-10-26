package shared

const (
	RuntimeActionGameInstallationObserve RuntimeAction = "runtime.game.installation.observe"
	RuntimeActionGameInstallationInstall RuntimeAction = "runtime.game.installation.install"
	RuntimeActionGameInstallationAdopt   RuntimeAction = "runtime.game.installation.adopt"
)

func IsGameInstallationAction(action RuntimeAction) bool {
	return IsSteamLoginAction(action) || action == RuntimeActionGameInstallationObserve || action == RuntimeActionGameInstallationInstall || action == RuntimeActionGameInstallationAdopt
}

// Paths are only accepted by explicit existing-installation inspection/adoption.
// Ordinary game operations continue to resolve the node's registered paths.
type GameInstallationRequest struct {
	CheckUsage  bool               `json:"checkUsage,omitempty"`
	Path        string             `json:"path,omitempty"`
	Fingerprint string             `json:"fingerprint,omitempty"`
	SteamLogin  *SteamLoginRequest `json:"steamLogin,omitempty"`
}

type GameInstallationReport struct {
	UsageState        string            `json:"usageState,omitempty"`
	UsageError        string            `json:"usageError,omitempty"`
	RunningProcesses  int               `json:"runningProcesses,omitempty"`
	SteamLogin        *SteamLoginStatus `json:"steamLogin,omitempty"`
	Installed         bool              `json:"installed"`
	ServerPath        string            `json:"serverPath"`
	ResolvedPath      string            `json:"resolvedPath,omitempty"`
	SavePath          string            `json:"savePath"`
	GameVersion       string            `json:"gameVersion,omitempty"`
	SteamCMDAvailable bool              `json:"steamcmdAvailable"`
	CanInstall        bool              `json:"canInstall"`
	CanAdopt          bool              `json:"canAdopt"`
	Reason            string            `json:"reason,omitempty"`
	Fingerprint       string            `json:"fingerprint,omitempty"`
}
