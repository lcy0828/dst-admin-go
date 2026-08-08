package capabilities

import (
	"fmt"
	"os"
	"path/filepath"
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
		toolCheck("luaFallback", "外部 Lua 回退", report.Tools["luaFallback"], false, "安装 Lua 可在内嵌解析器遇到兼容问题时自动回退"),
		toolCheck("mapRenderer", "地图渲染器", report.Tools["mapRenderer"], false, "配置 dst-map-renderer 后可生成分层地图；Session 诊断下载不受影响"),
		diskCheck(config.SavePath),
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
	case usage.Free < 2*gib:
		check.Status = CheckFail
		check.Summary = "剩余空间低于 2 GiB"
		check.Remediation = "清理磁盘或迁移存档后再执行开服、更新和备份"
	case usage.Free < 10*gib:
		check.Status = CheckWarning
		check.Summary = "剩余空间低于 10 GiB"
		check.Remediation = "建议先清理旧日志和备份"
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
