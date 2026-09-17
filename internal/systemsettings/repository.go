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
	Sensitive, Required, ReadOnly, RestartRequired             bool
	Minimum, Maximum                                           int
}

var fieldDefinitions = []fieldDefinition{
	{ID: "paths.save", Group: "paths", Label: "DST 存档目录", Kind: "path", Section: "paths", Key: "DST_SAVE_PATH", Environment: "DST_ADMIN_SAVE_PATH", Required: true, RestartRequired: true},
	{ID: "paths.backup", Group: "paths", Label: "备份目录", Kind: "path", Section: "paths", Key: "DST_BACKUP_PATH", Environment: "DST_ADMIN_BACKUP_PATH", Required: true, RestartRequired: true},
	{ID: "paths.server", Group: "paths", Label: "DST 服务端目录", Kind: "path", Section: "paths", Key: "DST_SERVER_PATH", Environment: "DST_ADMIN_SERVER_PATH", Required: true, RestartRequired: true},
	{ID: "paths.ugc", Group: "paths", Label: "UGC Mod 目录", Kind: "path", Section: "paths", Key: "DST_UGC_PATH", Environment: "DST_ADMIN_UGC_PATH", RestartRequired: true},
	{ID: "paths.map", Group: "paths", Label: "地图输出目录", Kind: "path", Section: "paths", Key: "DST_MAP_PATH", Environment: "DST_ADMIN_MAP_PATH", RestartRequired: true},
	{ID: "paths.serverMode", Group: "paths", Label: "服务端位数", Kind: "select", Section: "paths", Key: "DST_SERVER_MODE", Environment: "DST_ADMIN_SERVER_MODE", Default: "64", Options: []string{"32", "64"}, Required: true, RestartRequired: true},
	{ID: "mod.steamCMD", Group: "mod", Label: "SteamCMD 路径", Kind: "path", Section: "mod", Key: "STEAM_CMD_PATH", Environment: "DST_ADMIN_STEAMCMD_PATH", RestartRequired: true},
	{ID: "mod.workshopDownload", Group: "mod", Label: "Workshop 下载目录", Kind: "path", Section: "mod", Key: "WORKSHOP_MOD_PATH", Environment: "DST_ADMIN_WORKSHOP_DOWNLOAD", RestartRequired: true},
	{ID: "mod.workshopContent", Group: "mod", Label: "Workshop 内容目录", Kind: "path", Section: "mod", Key: "WORKSHOP_CONTENT", Environment: "DST_ADMIN_WORKSHOP_CONTENT", RestartRequired: true},
	{ID: "mod.steamAppID", Group: "mod", Label: "Steam App ID", Kind: "text", Section: "mod", Key: "APP_ID", Environment: "DST_ADMIN_STEAM_APP_ID", Default: "322330", Required: true, RestartRequired: true},
	{ID: "mod.steamAPIKey", Group: "mod", Label: "Steam Web API Key", Kind: "secret", Section: "mod", Key: "STEAM_WEB_API_KEY", Environment: "DST_ADMIN_STEAM_API_KEY", Sensitive: true, RestartRequired: true},
	{ID: "lua.binary", Group: "lua", Label: "外部 Lua 解释器", Kind: "text", Section: "mod", Key: "LUA_BINARY", Environment: "DST_ADMIN_LUA_BINARY", Default: "lua", Required: true, RestartRequired: true},
	{ID: "lua.pythonBinary", Group: "lua", Label: "Python/Lupa 解释器（可选）", Kind: "text", Section: "mod", Key: "PYTHON_BINARY", Environment: "DST_ADMIN_PYTHON_BINARY", Default: "python3", RestartRequired: true},
	{ID: "lua.fallback", Group: "lua", Label: "Lua 兼容模块目录（可选）", Kind: "path", Section: "mod", Key: "LUA_SH_PATH", Environment: "DST_ADMIN_LUA_PATH", RestartRequired: true},
	{ID: "map.renderer", Group: "map", Label: "地图渲染器路径", Kind: "path", Section: "map", Key: "RENDERER_PATH", Environment: "DST_ADMIN_MAP_RENDERER_PATH", RestartRequired: true},
	{ID: "misc.logLevel", Group: "misc", Label: "日志级别", Kind: "select", Section: "misc", Key: "LOG_LEVEL", Environment: "DST_ADMIN_LOG_LEVEL", Default: "info", Options: []string{"debug", "info", "warn", "error"}, Required: true, RestartRequired: true},
	{ID: "deployment.packaging", Group: "fleet", Label: "部署封装", Kind: "select", Section: "deployment", Key: "PACKAGING", Environment: "DST_ADMIN_PACKAGING", Default: "native", Options: []string{"native", "all_in_one", "container", "control_plane"}, Required: true, ReadOnly: true},
	{ID: "fleet.localExecutorEnabled", Group: "fleet", Label: "本地执行器", Kind: "boolean", Section: "fleet", Key: "LOCAL_EXECUTOR_ENABLED", Environment: "DST_ADMIN_LOCAL_EXECUTOR_ENABLED", Default: "true", Required: true, RestartRequired: true},
	{ID: "fleet.controllerEnabled", Group: "fleet", Label: "集中管理控制器", Kind: "boolean", Section: "fleet", Key: "CONTROLLER_ENABLED", Environment: "DST_ADMIN_FLEET_CONTROLLER_ENABLED", Default: "true", Required: true, RestartRequired: true},
	{ID: "fleet.memberEnabled", Group: "fleet", Label: "加入上级管理中心", Kind: "boolean", Section: "fleet", Key: "MEMBER_ENABLED", Environment: "DST_ADMIN_FLEET_MEMBER_ENABLED", Default: "false", Required: true, RestartRequired: true},
	{ID: "fleet.controllerUrl", Group: "fleet", Label: "上级管理中心地址", Kind: "text", Section: "agent", Key: "SERVER_URL", Environment: "DST_ADMIN_FLEET_CONTROLLER_URL", RestartRequired: true},
	{ID: "fleet.memberKey", Group: "fleet", Label: "节点连接密钥", Kind: "secret", Section: "agent", Key: "SECURITY_KEY", Environment: "DST_ADMIN_FLEET_MEMBER_KEY", Sensitive: true, RestartRequired: true},

	{ID: "ui.systemName", Group: "ui", Label: "管理系统名称", Kind: "text", Section: "ui", Key: "SYSTEM_NAME", Environment: "DST_ADMIN_SYSTEM_NAME", Default: "饥荒管理系统", Required: true},
	{ID: "ui.adminEmail", Group: "ui", Label: "管理员联系邮箱", Kind: "email", Section: "ui", Key: "ADMIN_EMAIL", Environment: "DST_ADMIN_ADMIN_EMAIL"},
	{ID: "ui.language", Group: "ui", Label: "系统语言", Kind: "select", Section: "ui", Key: "LANGUAGE", Environment: "DST_ADMIN_LANGUAGE", Default: "zh-CN", Options: []string{"zh-CN", "en-US"}, Required: true, ReadOnly: true},
	{ID: "ui.timezone", Group: "ui", Label: "时区设置", Kind: "select", Section: "ui", Key: "TIMEZONE", Environment: "DST_ADMIN_TIMEZONE", Default: "Asia/Shanghai", Options: []string{"Asia/Shanghai", "UTC", "America/Los_Angeles", "America/New_York", "Europe/Berlin", "Asia/Tokyo"}, Required: true},
	{ID: "ui.dateFormat", Group: "ui", Label: "日期格式", Kind: "select", Section: "ui", Key: "DATE_FORMAT", Environment: "DST_ADMIN_DATE_FORMAT", Default: "YYYY-MM-DD", Options: []string{"YYYY-MM-DD", "MM/DD/YYYY", "DD/MM/YYYY", "YYYY年MM月DD日"}, Required: true},
	{ID: "ui.theme", Group: "ui", Label: "主题颜色", Kind: "color", Section: "ui", Key: "THEME_COLOR", Environment: "DST_ADMIN_THEME_COLOR", Default: "#27272a", Required: true},

	{ID: "security.passwordComplexity", Group: "security", Label: "密码复杂度检查", Kind: "boolean", Section: "security", Key: "PASSWORD_COMPLEXITY", Environment: "DST_ADMIN_PASSWORD_COMPLEXITY", Default: "false", Required: true},
	{ID: "security.minPasswordLength", Group: "security", Label: "密码最小长度", Kind: "number", Section: "security", Key: "MIN_PASSWORD_LENGTH", Environment: "DST_ADMIN_MIN_PASSWORD_LENGTH", Default: "6", Required: true, Minimum: 6, Maximum: 20},
	{ID: "security.sessionTimeout", Group: "security", Label: "会话超时时间", Kind: "number", Section: "security", Key: "SESSION_TIMEOUT_MINUTES", Environment: "DST_ADMIN_SESSION_TIMEOUT", Default: "1440", Required: true, Minimum: 5, Maximum: 1440},
	{ID: "security.maxLoginAttempts", Group: "security", Label: "最大登录尝试次数", Kind: "number", Section: "security", Key: "MAX_LOGIN_ATTEMPTS", Environment: "DST_ADMIN_MAX_LOGIN_ATTEMPTS", Default: "5", Required: true, Minimum: 3, Maximum: 10},
	{ID: "security.twoFactorAuth", Group: "security", Label: "双因素认证", Kind: "boolean", Section: "security", Key: "TWO_FACTOR_AUTH", Default: "false", ReadOnly: true},
	{ID: "security.ipWhitelist", Group: "security", Label: "IP 白名单", Kind: "ip-list", Section: "security", Key: "IP_WHITELIST", Environment: "DST_ADMIN_IP_WHITELIST"},

	{ID: "backup.auto", Group: "backup", Label: "自动备份", Kind: "boolean", Section: "backup", Key: "AUTO_BACKUP", Environment: "DST_ADMIN_AUTO_BACKUP", Default: "true", Required: true},
	{ID: "backup.frequency", Group: "backup", Label: "备份频率", Kind: "select", Section: "backup", Key: "FREQUENCY", Environment: "DST_ADMIN_BACKUP_FREQUENCY", Default: "daily", Options: []string{"daily", "weekly", "monthly"}, Required: true},
	{ID: "backup.time", Group: "backup", Label: "备份时间", Kind: "time", Section: "backup", Key: "TIME", Environment: "DST_ADMIN_BACKUP_TIME", Default: "03:00", Required: true},
	{ID: "backup.retention", Group: "backup", Label: "保留备份数量", Kind: "number", Section: "backup", Key: "RETENTION", Environment: "DST_ADMIN_BACKUP_RETENTION", Default: "7", Required: true, Minimum: 1, Maximum: 100},

	{ID: "notification.emailEnabled", Group: "notification", Label: "邮件通知", Kind: "boolean", Section: "notification", Key: "EMAIL_ENABLED", Environment: "DST_ADMIN_EMAIL_ENABLED", Default: "false", Required: true},
	{ID: "notification.smtpServer", Group: "notification", Label: "SMTP 服务器", Kind: "text", Section: "notification", Key: "SMTP_SERVER", Environment: "DST_ADMIN_SMTP_SERVER"},
	{ID: "notification.smtpPort", Group: "notification", Label: "SMTP 端口", Kind: "number", Section: "notification", Key: "SMTP_PORT", Environment: "DST_ADMIN_SMTP_PORT", Default: "587", Minimum: 1, Maximum: 65535},
	{ID: "notification.smtpUsername", Group: "notification", Label: "SMTP 用户名", Kind: "text", Section: "notification", Key: "SMTP_USERNAME", Environment: "DST_ADMIN_SMTP_USERNAME"},
	{ID: "notification.smtpPassword", Group: "notification", Label: "SMTP 密码", Kind: "secret", Section: "notification", Key: "SMTP_PASSWORD", Environment: "DST_ADMIN_SMTP_PASSWORD", Sensitive: true},
	{ID: "notification.senderEmail", Group: "notification", Label: "发件人邮箱", Kind: "email", Section: "notification", Key: "SENDER_EMAIL", Environment: "DST_ADMIN_SENDER_EMAIL"},
	{ID: "notification.serverStatus", Group: "notification", Label: "服务器状态通知", Kind: "boolean", Section: "notification", Key: "SERVER_STATUS", Environment: "DST_ADMIN_NOTIFY_SERVER_STATUS", Default: "true", Required: true},
	{ID: "notification.loginFailures", Group: "notification", Label: "登录失败通知", Kind: "boolean", Section: "notification", Key: "LOGIN_FAILURES", Environment: "DST_ADMIN_NOTIFY_LOGIN_FAILURES", Default: "true", Required: true},
	{ID: "notification.backupResults", Group: "notification", Label: "备份结果通知", Kind: "boolean", Section: "notification", Key: "BACKUP_RESULTS", Environment: "DST_ADMIN_NOTIFY_BACKUP_RESULTS", Default: "true", Required: true},
	{ID: "notification.systemUpdates", Group: "notification", Label: "系统更新通知", Kind: "boolean", Section: "notification", Key: "SYSTEM_UPDATES", Environment: "DST_ADMIN_NOTIFY_SYSTEM_UPDATES", Default: "true", Required: true},
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
		"paths.save": "/opt/dst/saves", "paths.backup": "/opt/dst/backups", "paths.server": "/opt/dst/server", "paths.ugc": "/opt/dst/workshop/steamapps/workshop", "paths.map": "/opt/dst/maps", "paths.serverMode": "64",
		"mod.steamCMD": "/usr/local/bin/steamcmd", "mod.workshopDownload": "/opt/dst/workshop", "mod.workshopContent": "/opt/dst/workshop/steamapps/workshop/content/322330", "mod.steamAppID": "322330", "mod.steamAPIKey": "test-steam-api-key",
		"lua.binary": "lua", "lua.pythonBinary": "python3", "lua.fallback": "/opt/dst/lua", "map.renderer": "/opt/dst/bin/map-renderer", "misc.logLevel": "info",
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
