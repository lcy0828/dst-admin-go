package setting

import (
	"os"
	"strings"

	"github.com/go-ini/ini"
)

// Snapshot gives each application generation its own immutable runtime config.
// Reloading never replaces the legacy process-wide Cfg or the database settings.
type Snapshot struct {
	file       *ini.File
	ConfigPath string
}

func CurrentSnapshot() Snapshot { return Snapshot{file: Cfg, ConfigPath: ConfigPath} }

func (s Snapshot) Reload() (Snapshot, error) {
	file, err := ini.Load(s.ConfigPath)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{file: file, ConfigPath: s.ConfigPath}, nil
}

func (s Snapshot) String(section, key, environment string) string {
	if value := strings.TrimSpace(os.Getenv(environment)); value != "" {
		return value
	}
	return s.file.Section(section).Key(key).String()
}

func (s Snapshot) Path(section, key, environment string) string {
	return ResolvePath(s.String(section, key, environment))
}
