package configuration

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrRoomNotManaged       = errors.New("room must be managed before configuration can be changed")
	ErrRevisionConflict     = errors.New("configuration revision has changed")
	ErrNoChanges            = errors.New("configuration has no changes")
	ErrConfirmationNeeded   = errors.New("exact room name confirmation is required")
	ErrInvalidConfiguration = errors.New("configuration is invalid")
	ErrUnsafePath           = errors.New("configuration path is unsafe")
	ErrFileTooLarge         = errors.New("configuration file exceeds the size limit")
	ErrUnsupportedLuaValue  = errors.New("leveldataoverride contains an unsupported Lua value")
)

type FieldError struct {
	Fields map[string]string
}

func (e *FieldError) Error() string { return ErrInvalidConfiguration.Error() }
func (e *FieldError) Unwrap() error { return ErrInvalidConfiguration }

type RevisionConflictError struct {
	CurrentRevision string
}

func (e *RevisionConflictError) Error() string { return ErrRevisionConflict.Error() }
func (e *RevisionConflictError) Unwrap() error { return ErrRevisionConflict }

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type FieldSchema struct {
	Key         string   `json:"key"`
	Group       string   `json:"group"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
	Minimum     *int     `json:"minimum,omitempty"`
	Maximum     *int     `json:"maximum,omitempty"`
	Options     []Option `json:"options,omitempty"`
}

type Change struct {
	Path      string      `json:"path"`
	Label     string      `json:"label"`
	Before    interface{} `json:"before,omitempty"`
	After     interface{} `json:"after,omitempty"`
	Sensitive bool        `json:"sensitive,omitempty"`
	Operation string      `json:"operation"`
}

type Preview struct {
	Revision             string   `json:"revision"`
	NextRevision         string   `json:"nextRevision"`
	Changes              []Change `json:"changes"`
	RequiresConfirmation bool     `json:"requiresConfirmation"`
}

type ApplyResult struct {
	Revision           string   `json:"revision"`
	Changes            []Change `json:"changes"`
	ProtectionBackupID string   `json:"protectionBackupId"`
}

type RoomValues struct {
	ClusterName        string `json:"clusterName"`
	ClusterDescription string `json:"clusterDescription"`
	ClusterPassword    string `json:"clusterPassword"`
	GameMode           string `json:"gameMode"`
	MaxPlayers         int    `json:"maxPlayers"`
	PvP                bool   `json:"pvp"`
	PauseWhenEmpty     bool   `json:"pauseWhenEmpty"`
	VoteEnabled        bool   `json:"voteEnabled"`
	ConsoleEnabled     bool   `json:"consoleEnabled"`
	LANOnly            bool   `json:"lanOnly"`
	Offline            bool   `json:"offline"`
}

type RoomConfig struct {
	Revision          string        `json:"revision"`
	Values            RoomValues    `json:"values"`
	Schema            []FieldSchema `json:"schema"`
	UnknownFieldCount int           `json:"unknownFieldCount"`
	ModifiedAt        time.Time     `json:"modifiedAt"`
}

type RoomUpdateRequest struct {
	ExpectedRevision string     `json:"expectedRevision"`
	Values           RoomValues `json:"values"`
}

type WorldServerValues struct {
	ServerPort         int  `json:"serverPort"`
	AuthenticationPort int  `json:"authenticationPort"`
	MasterServerPort   int  `json:"masterServerPort"`
	EncodeUserPath     bool `json:"encodeUserPath"`
}

type OverrideSchema struct {
	Key         string   `json:"key"`
	Group       string   `json:"group"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Options     []Option `json:"options,omitempty"`
}

type WorldConfig struct {
	Revision          string                 `json:"revision"`
	Server            WorldServerValues      `json:"server"`
	ServerSchema      []FieldSchema          `json:"serverSchema"`
	Overrides         map[string]interface{} `json:"overrides"`
	OverrideSchema    []OverrideSchema       `json:"overrideSchema"`
	UnknownFieldCount int                    `json:"unknownFieldCount"`
	ModifiedAt        time.Time              `json:"modifiedAt"`
}

type WorldUpdateRequest struct {
	ExpectedRevision string                     `json:"expectedRevision"`
	Server           WorldServerValues          `json:"server"`
	OverridePatch    map[string]json.RawMessage `json:"overridePatch"`
}

type AccessLists struct {
	Revision  string   `json:"revision"`
	Admins    []string `json:"admins"`
	Blocked   []string `json:"blocked"`
	Whitelist []string `json:"whitelist"`
}

type AccessUpdateRequest struct {
	ExpectedRevision string   `json:"expectedRevision"`
	Admins           []string `json:"admins"`
	Blocked          []string `json:"blocked"`
	Whitelist        []string `json:"whitelist"`
	Confirmation     string   `json:"confirmation"`
}

type TokenStatus struct {
	Revision    string `json:"revision"`
	Configured  bool   `json:"configured"`
	MaskedValue string `json:"maskedValue"`
}

type TokenRevealRequest struct {
	Confirmation string `json:"confirmation"`
}

type TokenReveal struct {
	Revision string `json:"revision"`
	Token    string `json:"token"`
}

type TokenUpdateRequest struct {
	ExpectedRevision string `json:"expectedRevision"`
	Token            string `json:"token"`
	Confirmation     string `json:"confirmation"`
}
