package systemsettings

import (
	"errors"
	"time"
)

var (
	ErrConflict             = errors.New("system settings revision conflict")
	ErrInvalidInput         = errors.New("system settings input is invalid")
	ErrConfirmationRequired = errors.New("system settings confirmation is required")
)

const ApplyConfirmation = "APPLY SYSTEM SETTINGS"

type FieldSource string

const (
	SourceFile        FieldSource = "file"
	SourceEnvironment FieldSource = "environment"
	SourceDefault     FieldSource = "default"
)

type Field struct {
	ID              string      `json:"id"`
	Group           string      `json:"group"`
	Label           string      `json:"label"`
	Kind            string      `json:"kind"`
	Value           string      `json:"value"`
	Options         []string    `json:"options"`
	Source          FieldSource `json:"source"`
	Environment     string      `json:"environment,omitempty"`
	Editable        bool        `json:"editable"`
	Sensitive       bool        `json:"sensitive"`
	Configured      bool        `json:"configured"`
	RestartRequired bool        `json:"restartRequired"`
	Minimum         int         `json:"minimum,omitempty"`
	Maximum         int         `json:"maximum,omitempty"`
}

type Settings struct {
	Revision          string    `json:"revision"`
	ConfigurationPath string    `json:"configurationPath"`
	BackupPath        string    `json:"backupPath"`
	RestartRequired   bool      `json:"restartRequired"`
	Fields            []Field   `json:"fields"`
	ReadAt            time.Time `json:"readAt"`
}

type Input struct {
	Revision     string            `json:"revision"`
	Values       map[string]string `json:"values"`
	ClearSecrets []string          `json:"clearSecrets"`
	Confirmation string            `json:"confirmation,omitempty"`
}

type Change struct {
	FieldID   string `json:"fieldId"`
	Label     string `json:"label"`
	Before    string `json:"before"`
	After     string `json:"after"`
	Sensitive bool   `json:"sensitive"`
}

type Issue struct {
	FieldID  string `json:"fieldId,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type Preview struct {
	Valid           bool     `json:"valid"`
	Revision        string   `json:"revision"`
	Changes         []Change `json:"changes"`
	Issues          []Issue  `json:"issues"`
	RestartRequired bool     `json:"restartRequired"`
}

type ApplyResult struct {
	Settings Settings `json:"settings"`
	Changes  []Change `json:"changes"`
}

type RuntimeSettings struct {
	SystemName          string
	AdminEmail          string
	Language            string
	Timezone            string
	DateFormat          string
	Theme               string
	PasswordComplexity  bool
	MinPasswordLength   int
	SessionTimeout      time.Duration
	MaxLoginAttempts    int
	IPWhitelist         string
	AutoBackup          bool
	BackupFrequency     string
	BackupTime          string
	BackupRetention     int
	EmailEnabled        bool
	SMTPServer          string
	SMTPPort            int
	SMTPUsername        string
	SMTPPassword        string
	SenderEmail         string
	NotifyServerStatus  bool
	NotifyLoginFailures bool
	NotifyBackupResults bool
	NotifySystemUpdates bool
}

type SMTPTestInput struct {
	Server            string `json:"server" binding:"required"`
	Port              int    `json:"port" binding:"required"`
	Username          string `json:"username"`
	Password          string `json:"password"`
	UseStoredPassword bool   `json:"useStoredPassword"`
}

type SMTPTestResult struct {
	Server        string          `json:"server"`
	TLS           bool            `json:"tls"`
	Authenticated bool            `json:"authenticated"`
	Stages        []SMTPTestStage `json:"stages"`
}

type SMTPTestStage struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}
