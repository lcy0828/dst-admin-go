package capabilities

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	dstinstall "dont/internal/dstserver"

	"github.com/shirou/gopsutil/v3/disk"
)

type CheckStatus string

const (
	CheckPass    CheckStatus = "pass"
	CheckWarning CheckStatus = "warning"
	CheckFail    CheckStatus = "fail"
)

type Check struct {
	ID          string                 `json:"id"`
	Label       string                 `json:"label"`
	Status      CheckStatus            `json:"status"`
	Required    bool                   `json:"required"`
	Summary     string                 `json:"summary"`
	Remediation string                 `json:"remediation,omitempty"`
	Details     map[string]interface{} `json:"details,omitempty"`
}

type Readiness struct {
	Ready  bool    `json:"ready"`
	Checks []Check `json:"checks"`
}

type RoomCounter func() (int, error)

func ProbeReadiness(config Config, countRooms RoomCounter) Readiness {
	report := Probe(config)
	checks := []Check{
		pathCheck("savePath", "DST 存档目录", report.Paths["saves"], true),
		pathCheck("backupPath", "备份目录", report.Paths["backups"], true),
		toolCheck("tmux", "tmux 运行控制", report.Tools["tmux"], true, "安装 tmux 后才能在本机启动和停止分片"),
		serverExecutableCheck(config.ServerPath, config.ServerMode),
		toolCheck("steamcmd", "SteamCMD", report.Tools["steamcmd"], false, "配置 SteamCMD 后才能执行游戏更新和 Workshop 下载"),
		luaFallbackCheck(config, report.Tools["luaFallback"]),
		toolCheck("mapRenderer", "地图渲染器", report.Tools["mapRenderer"], false, "配置 dst-map-renderer 后可生成分层地图；Session 诊断下载不受影响"),
		diskCheck(config.SavePath),
	}
	if check, relevant := macSteamRuntimeCheck(config.ServerPath, config.ServerMode); relevant {
		checks = append(checks, check)
	}
	roomCheck := Check{ID: "rooms", Label: "已有房间", Required: false, Status: CheckPass, Summary: "未发现已有房间，可直接创建新房间"}
	if countRooms != nil {
		count, err := countRooms()
		if err != nil {
			roomCheck.Status = CheckWarning
			roomCheck.Summary = "暂时无法扫描已有房间"
			roomCheck.Remediation = "检查存档目录权限和 cluster.ini 文件"
		} else if count > 0 {
			roomCheck.Summary = fmt.Sprintf("发现 %d 个可接管房间", count)
			roomCheck.Details = map[string]interface{}{"count": count}
		}
	}
	checks = append(checks, roomCheck)
	ready := true
	for _, check := range checks {
		if check.Required && check.Status == CheckFail {
			ready = false
			break
		}
	}
	return Readiness{Ready: ready, Checks: checks}
}

func luaFallbackCheck(config Config, tool Tool) Check {
	check := Check{
		ID: "luaFallback", Label: "Mod 兼容 fallback", Required: false,
		Details: map[string]interface{}{
			"configuredLuaBinary": config.LuaBinary, "configuredPythonBinary": config.PythonBinary,
			"modulePath": strings.TrimSpace(config.LuaFallbackPath), "diagnostic": tool.Diagnostic,
		},
	}
	if tool.Available {
		check.Status = CheckPass
		check.Summary = "兼容 fallback 可用（" + tool.Kind + "）"
		check.Details["path"] = tool.Path
		check.Details["source"] = tool.Source
		check.Details["version"] = tool.Version
	} else {
		check.Status = CheckWarning
		check.Summary = "未发现外部 fallback；Go 主解析器仍可用"
		check.Remediation = fallbackRemediation()
	}

	modulePath := strings.TrimSpace(config.LuaFallbackPath)
	if modulePath == "" {
		check.Details["modulePathConfigured"] = false
		return check
	}
	check.Details["modulePathConfigured"] = true
	info, err := os.Stat(modulePath)
	moduleAvailable := err == nil && info.IsDir()
	check.Details["modulePathAvailable"] = moduleAvailable
	if moduleAvailable {
		return check
	}
	if tool.Available {
		check.Status = CheckWarning
		check.Summary = "解释器可用，但可选 Lua 兼容模块目录不存在"
	}
	moduleRemediation := "清空 DST_ADMIN_LUA_PATH，或将它指向确实存在的 Lua/C 模块目录；内嵌 fallback helper 不依赖该目录"
	if check.Remediation == "" {
		check.Remediation = moduleRemediation
	} else {
		check.Remediation += "；" + moduleRemediation
	}
	return check
}

func fallbackRemediation() string {
	if runtime.GOOS == "darwin" {
		return "安装 Homebrew Lua（brew install lua），或为独立 Python 环境安装 Lupa 并通过 DST_ADMIN_PYTHON_BINARY 指定解释器"
	}
	return "安装 Lua 并通过 DST_ADMIN_LUA_BINARY 指定解释器，或为独立 Python 环境安装 Lupa 并配置 DST_ADMIN_PYTHON_BINARY"
}

func macSteamRuntimeCheck(serverPath, serverMode string) (Check, bool) {
	layout, ok := dstinstall.Resolve(serverPath, serverMode)
	if !ok || layout.Kind != dstinstall.LayoutMac {
		return Check{}, false
	}
	check := Check{ID: "steamClientLibrary", Label: "macOS Steam 运行库", Required: false}
	if directory := dstinstall.SteamClientLibraryDirectory(layout); directory != "" {
		check.Status = CheckPass
		check.Summary = "steamclient.dylib 可用"
		check.Details = map[string]interface{}{"path": directory}
		return check, true
	}
	check.Status = CheckWarning
	check.Summary = "未找到 steamclient.dylib"
	check.Remediation = "启动 Steam 客户端，或通过 DST_ADMIN_STEAM_CLIENT_LIBRARY_PATH 配置动态库目录"
	return check, true
}

func pathCheck(id, label string, path Path, required bool) Check {
	check := Check{ID: id, Label: label, Required: required, Details: map[string]interface{}{"path": path.Value}}
	switch {
	case !path.Configured:
		check.Status = CheckFail
		check.Summary = "尚未配置路径"
		check.Remediation = "在 conf/app.conf 的 paths 段配置该目录"
	case !path.Exists && path.Writable:
		check.Status = CheckWarning
		check.Summary = "目录尚未创建，但父目录可写"
		check.Remediation = "创建首个房间时系统会建立目录"
	case !path.Exists:
		check.Status = CheckFail
		check.Summary = "目录不存在且无法创建"
		check.Remediation = "创建目录并授予服务账号读写权限"
	case !path.Writable:
		check.Status = CheckFail
		check.Summary = "目录存在但不可写"
		check.Remediation = "授予服务账号目录读写权限"
	default:
		check.Status = CheckPass
		check.Summary = "目录可读写"
	}
	return check
}

func toolCheck(id, label string, tool Tool, required bool, remediation string) Check {
	check := Check{ID: id, Label: label, Required: required, Details: map[string]interface{}{"path": tool.Path}}
	if tool.Available {
		check.Status = CheckPass
		check.Summary = "工具可用"
		return check
	}
	if required {
		check.Status = CheckFail
	} else {
		check.Status = CheckWarning
	}
	check.Summary = "未找到工具"
	check.Remediation = remediation
	return check
}

func serverExecutableCheck(serverPath, serverMode string) Check {
	check := Check{ID: "serverExecutable", Label: "DST 服务端", Required: true, Details: map[string]interface{}{"configuredPath": serverPath}}
	layout, ok := dstinstall.Resolve(serverPath, serverMode)
	if !ok {
		check.Status = CheckFail
		check.Summary = "未找到 DST 专用服务器可执行文件"
		check.Remediation = "把 DST_SERVER_PATH 指向安装目录；macOS 可直接指向 Don't Starve Together 目录或 .app"
		return check
	}
	check.Status = CheckPass
	check.Summary = "服务端可执行文件可用"
	check.Details["executable"] = layout.Executable
	check.Details["layout"] = layout.Kind
	check.Details["contentRoot"] = layout.ContentRoot
	check.Details["appId"] = layout.AppID
	check.Details["updateMethod"] = layout.UpdateMethod
	check.Details["updateSupported"] = layout.UpdateSupported
	return check
}

func findDSTExecutable(serverPath string, serverMode ...string) string {
	mode := "64"
	if len(serverMode) > 0 {
		mode = serverMode[0]
	}
	layout, ok := dstinstall.Resolve(serverPath, mode)
	if !ok {
		return ""
	}
	return layout.Executable
}

func diskCheck(savePath string) Check {
	check := Check{ID: "diskSpace", Label: "存档磁盘空间", Required: true}
	probePath := nearestExistingDirectory(savePath)
	if probePath == "" {
		check.Status = CheckFail
		check.Summary = "无法确定存档磁盘空间"
		check.Remediation = "检查 DST_SAVE_PATH 及其父目录"
		return check
	}
	usage, err := disk.Usage(probePath)
	if err != nil {
		check.Status = CheckWarning
		check.Summary = "读取磁盘空间失败"
		check.Remediation = "确认服务账号有权读取文件系统信息"
		return check
	}
	const gib = uint64(1024 * 1024 * 1024)
	check.Details = map[string]interface{}{
		"path": probePath, "freeBytes": usage.Free, "totalBytes": usage.Total, "usedPercent": usage.UsedPercent,
	}
	switch {
	case usage.Free < 2*gib || usage.UsedPercent >= 95:
		check.Status = CheckFail
		check.Summary = "磁盘空间已达到危险阈值"
		check.Remediation = "清理磁盘或迁移存档后再执行开服、更新和备份"
	case usage.Free < 10*gib || usage.UsedPercent >= 80:
		check.Status = CheckWarning
		check.Summary = "磁盘使用率较高"
		check.Remediation = "建议清理旧日志和备份，并保持至少 20% 可用空间"
	default:
		check.Status = CheckPass
		check.Summary = "磁盘空间充足"
	}
	return check
}

func nearestExistingDirectory(value string) string {
	value = filepath.Clean(strings.TrimSpace(value))
	if value == "" || value == "." {
		return ""
	}
	for {
		info, err := os.Stat(value)
		if err == nil {
			if info.IsDir() {
				return value
			}
			return filepath.Dir(value)
		}
		parent := filepath.Dir(value)
		if parent == value {
			return ""
		}
		value = parent
	}
}
