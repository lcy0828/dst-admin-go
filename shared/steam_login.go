package shared

import "time"

const (
	SteamLoginCapability   = "runtime.steam-login.v1"
	SteamLoginV2Capability = "runtime.steam-login.v2"
)

const (
	RuntimeActionSteamLoginObserve RuntimeAction = "runtime.game.installation.steam-login.observe"
	RuntimeActionSteamLoginManage  RuntimeAction = "runtime.game.installation.steam-login.manage"
)

// Password and Code are write-only and must never be included in reports or jobs.
type SteamLoginRequest struct {
	Action               string `json:"action"`
	Mode                 string `json:"mode,omitempty"`
	Username             string `json:"username,omitempty"`
	Password             string `json:"password,omitempty"`
	UseCachedCredentials bool   `json:"useCachedCredentials,omitempty"`
	SessionID            string `json:"sessionId,omitempty"`
	Code                 string `json:"code,omitempty"`
}

func (r SteamLoginRequest) RequiresV2Capability() bool {
	return r.UseCachedCredentials || (r.Action == "login" && r.Code != "")
}

type SteamLoginStatus struct {
	Mode             string     `json:"mode"`
	Username         string     `json:"username,omitempty"`
	PasswordSet      bool       `json:"passwordSet"`
	RecoveryRequired bool       `json:"recoveryRequired,omitempty"`
	State            string     `json:"state"`
	SessionID        string     `json:"sessionId,omitempty"`
	ErrorCode        string     `json:"errorCode,omitempty"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
}

func IsSteamLoginAction(action RuntimeAction) bool {
	return action == RuntimeActionSteamLoginObserve || action == RuntimeActionSteamLoginManage
}
