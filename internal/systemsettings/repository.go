package systemsettings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-ini/ini"
)

type fieldDefinition struct {
	ID, Group, Label, Kind, Section, Key, Environment, Default string
	Options                                                    []string
	Sensitive, Required                                        bool
}

var fieldDefinitions = []fieldDefinition{
	{ID: "paths.save", Group: "paths", Label: "DST 存档目录", Kind: "path", Section: "paths", Key: "DST_SAVE_PATH", Environment: "DST_ADMIN_SAVE_PATH", Required: true},
	{ID: "paths.backup", Group: "paths", Label: "备份目录", Kind: "path", Section: "paths", Key: "DST_BACKUP_PATH", Environment: "DST_ADMIN_BACKUP_PATH", Required: true},
	{ID: "paths.server", Group: "paths", Label: "DST 服务端目录", Kind: "path", Section: "paths", Key: "DST_SERVER_PATH", Environment: "DST_ADMIN_SERVER_PATH", Required: true},
	{ID: "paths.ugc", Group: "paths", Label: "UGC Mod 目录", Kind: "path", Section: "paths", Key: "DST_UGC_PATH", Environment: "DST_ADMIN_UGC_PATH"},
	{ID: "paths.map", Group: "paths", Label: "地图输出目录", Kind: "path", Section: "paths", Key: "DST_MAP_PATH", Environment: "DST_ADMIN_MAP_PATH"},
	{ID: "paths.serverMode", Group: "paths", Label: "服务端位数", Kind: "select", Section: "paths", Key: "DST_SERVER_MODE", Environment: "DST_ADMIN_SERVER_MODE", Default: "64", Options: []string{"32", "64"}, Required: true},
	{ID: "mod.steamCMD", Group: "mod", Label: "SteamCMD 路径", Kind: "path", Section: "mod", Key: "STEAM_CMD_PATH", Environment: "DST_ADMIN_STEAMCMD_PATH"},
	{ID: "mod.workshopDownload", Group: "mod", Label: "Workshop 下载目录", Kind: "path", Section: "mod", Key: "WORKSHOP_MOD_PATH", Environment: "DST_ADMIN_WORKSHOP_DOWNLOAD"},
	{ID: "mod.workshopContent", Group: "mod", Label: "Workshop 内容目录", Kind: "path", Section: "mod", Key: "WORKSHOP_CONTENT", Environment: "DST_ADMIN_WORKSHOP_CONTENT"},
	{ID: "mod.steamAppID", Group: "mod", Label: "Steam App ID", Kind: "text", Section: "mod", Key: "APP_ID", Environment: "DST_ADMIN_STEAM_APP_ID", Default: "322330", Required: true},
	{ID: "mod.steamAPIKey", Group: "mod", Label: "Steam Web API Key", Kind: "secret", Section: "mod", Key: "STEAM_WEB_API_KEY", Environment: "DST_ADMIN_STEAM_API_KEY", Sensitive: true},
	{ID: "lua.binary", Group: "lua", Label: "外部 Lua 解释器", Kind: "text", Section: "mod", Key: "LUA_BINARY", Environment: "DST_ADMIN_LUA_BINARY", Default: "lua", Required: true},
	{ID: "lua.fallback", Group: "lua", Label: "Lua 兼容模块目录（可选）", Kind: "path", Section: "mod", Key: "LUA_SH_PATH", Environment: "DST_ADMIN_LUA_PATH"},
	{ID: "map.renderer", Group: "map", Label: "地图渲染器路径", Kind: "path", Section: "map", Key: "RENDERER_PATH", Environment: "DST_ADMIN_MAP_RENDERER_PATH"},
	{ID: "misc.logLevel", Group: "misc", Label: "日志级别", Kind: "select", Section: "misc", Key: "LOG_LEVEL", Environment: "DST_ADMIN_LOG_LEVEL", Default: "info", Options: []string{"debug", "info", "warn", "error"}, Required: true},
}

type Snapshot struct {
	Revision          string
	ConfigurationPath string
	BackupPath        string
	Values            map[string]string
}

type Repository interface {
	Snapshot() (Snapshot, error)
	Save(expectedRevision string, values map[string]string) (Snapshot, error)
}

type FileRepository struct{ path string }

func NewFileRepository(path string) *FileRepository {
	return &FileRepository{path: filepath.Clean(path)}
}

func (r *FileRepository) Snapshot() (Snapshot, error) {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotFromBytes(r.path, raw)
}

func (r *FileRepository) Save(expectedRevision string, values map[string]string) (Snapshot, error) {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return Snapshot{}, err
	}
	current, err := snapshotFromBytes(r.path, raw)
	if err != nil {
		return Snapshot{}, err
	}
	if current.Revision != expectedRevision {
		return Snapshot{}, ErrConflict
	}
	configuration, err := ini.Load(raw)
	if err != nil {
		return Snapshot{}, err
	}
	for id, value := range values {
		definition, ok := definitionByID(id)
		if !ok {
			return Snapshot{}, ErrInvalidInput
		}
		configuration.Section(definition.Section).Key(definition.Key).SetValue(value)
	}
	var encoded bytes.Buffer
	if _, err := configuration.WriteTo(&encoded); err != nil {
		return Snapshot{}, err
	}
	if err := atomicWrite(r.path+".bak", raw, 0600); err != nil {
		return Snapshot{}, fmt.Errorf("backup settings: %w", err)
	}
	if err := atomicWrite(r.path, encoded.Bytes(), 0600); err != nil {
		return Snapshot{}, fmt.Errorf("write settings: %w", err)
	}
	return r.Snapshot()
}

func snapshotFromBytes(path string, raw []byte) (Snapshot, error) {
	configuration, err := ini.Load(raw)
	if err != nil {
		return Snapshot{}, err
	}
	values := make(map[string]string, len(fieldDefinitions))
	for _, definition := range fieldDefinitions {
		values[definition.ID] = configuration.Section(definition.Section).Key(definition.Key).String()
	}
	return Snapshot{Revision: digest(raw), ConfigurationPath: path, BackupPath: path + ".bak", Values: values}, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".dst-admin-settings-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

type MemoryRepository struct {
	mu     sync.Mutex
	values map[string]string
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{values: map[string]string{
		"paths.save": "/srv/dst/saves", "paths.backup": "/srv/dst/backups", "paths.server": "/srv/dst/server", "paths.ugc": "/srv/dst/ugc", "paths.map": "/srv/dst/maps", "paths.serverMode": "64",
		"mod.steamCMD": "/usr/local/bin/steamcmd", "mod.workshopDownload": "/srv/steam", "mod.workshopContent": "/srv/steam/steamapps/workshop/content/322330", "mod.steamAppID": "322330", "mod.steamAPIKey": "test-steam-api-key",
		"lua.binary": "lua", "lua.fallback": "/srv/dst/lua", "map.renderer": "/srv/dst/bin/map-renderer", "misc.logLevel": "info",
	}}
}

func (r *MemoryRepository) Snapshot() (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return memorySnapshot(r.values), nil
}

func (r *MemoryRepository) Save(expectedRevision string, values map[string]string) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := memorySnapshot(r.values)
	if current.Revision != expectedRevision {
		return Snapshot{}, ErrConflict
	}
	for id, value := range values {
		if _, ok := definitionByID(id); !ok {
			return Snapshot{}, ErrInvalidInput
		}
		r.values[id] = value
	}
	return memorySnapshot(r.values), nil
}

func memorySnapshot(values map[string]string) Snapshot {
	copyValues := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key, value := range values {
		copyValues[key] = value
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var source strings.Builder
	for _, key := range keys {
		source.WriteString(key)
		source.WriteByte(0)
		source.WriteString(values[key])
		source.WriteByte(0)
	}
	return Snapshot{Revision: digest([]byte(source.String())), ConfigurationPath: "memory://system-settings", BackupPath: "memory://system-settings-backup", Values: copyValues}
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func definitionByID(id string) (fieldDefinition, bool) {
	for _, definition := range fieldDefinitions {
		if definition.ID == id {
			return definition, true
		}
	}
	return fieldDefinition{}, false
}
