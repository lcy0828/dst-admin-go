package deploymentprofile

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"dont/shared"
)

type Packaging string

const (
	PackagingNative       Packaging = "native"
	PackagingAllInOne     Packaging = "all_in_one"
	PackagingControlPlane Packaging = "control_plane"
)

type Role string

const (
	RoleStandalone       Role = "standalone"
	RoleControllerWorker Role = "controller_worker"
	RoleManagedWorker    Role = "managed_worker"
	RoleControllerOnly   Role = "controller_only"
)

var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Values struct {
	Packaging            string
	LocalExecutorEnabled string
	ControllerEnabled    string
	MemberEnabled        string
	ControllerURL        string
	MemberKey            string
	NodeID               string
	StatePath            string
}

type Profile struct {
	Packaging            Packaging `json:"packaging"`
	Role                 Role      `json:"role"`
	LocalExecutorEnabled bool      `json:"localExecutorEnabled"`
	ControllerEnabled    bool      `json:"controllerEnabled"`
	MemberEnabled        bool      `json:"memberEnabled"`
	ControllerURL        string    `json:"controllerUrl,omitempty"`
	MemberKeyConfigured  bool      `json:"memberKeyConfigured"`
	NodeID               string    `json:"nodeId,omitempty"`
	StatePath            string    `json:"statePath,omitempty"`
	MemberKey            string    `json:"-"`
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func Resolve(values Values) (Profile, error) {
	packaging, err := parsePackaging(values.Packaging)
	if err != nil {
		return Profile{}, err
	}
	localExecutor, err := parseBool("fleet.localExecutorEnabled", values.LocalExecutorEnabled, true)
	if err != nil {
		return Profile{}, err
	}
	controller, err := parseBool("fleet.controllerEnabled", values.ControllerEnabled, true)
	if err != nil {
		return Profile{}, err
	}
	member, err := parseBool("fleet.memberEnabled", values.MemberEnabled, false)
	if err != nil {
		return Profile{}, err
	}
	if packaging == PackagingControlPlane && localExecutor {
		return Profile{}, invalid("fleet.localExecutorEnabled", "独立控制端不能启用本地 DST 执行器")
	}
	if packaging == PackagingControlPlane && !controller {
		return Profile{}, invalid("fleet.controllerEnabled", "独立控制端必须启用集中管理控制器")
	}
	if controller && member {
		return Profile{}, invalid("fleet.memberEnabled", "同一实例不能同时加入上级管理中心并接受下级节点")
	}
	if member && !localExecutor {
		return Profile{}, invalid("fleet.localExecutorEnabled", "受管工作节点必须启用本地执行器")
	}
	if !localExecutor && !controller {
		return Profile{}, invalid("fleet.localExecutorEnabled", "至少需要启用本地执行器或集中管理控制器")
	}

	controllerURL := strings.TrimSpace(values.ControllerURL)
	memberKey := strings.TrimSpace(values.MemberKey)
	statePath := strings.TrimSpace(values.StatePath)
	nodeID := strings.TrimSpace(values.NodeID)
	if nodeID != "" && !nodeIDPattern.MatchString(nodeID) {
		return Profile{}, invalid("fleet.nodeId", "节点 ID 只能包含字母、数字、点、下划线和连字符")
	}
	if member {
		controllerURL, err = normalizeControllerURL(controllerURL)
		if err != nil {
			return Profile{}, err
		}
		if err := shared.ValidateSecurityKey(memberKey); err != nil {
			return Profile{}, invalid("fleet.memberKey", "加入管理中心需要有效的节点连接密钥")
		}
		if statePath != "" && (!filepath.IsAbs(statePath) || strings.ContainsAny(statePath, "\x00\r\n")) {
			return Profile{}, invalid("fleet.statePath", "Fleet 状态目录必须使用绝对路径")
		}
	} else {
		controllerURL = strings.TrimSpace(controllerURL)
	}

	role := RoleStandalone
	switch {
	case member:
		role = RoleManagedWorker
	case controller && localExecutor:
		role = RoleControllerWorker
	case controller:
		role = RoleControllerOnly
	}
	return Profile{
		Packaging: packaging, Role: role, LocalExecutorEnabled: localExecutor,
		ControllerEnabled: controller, MemberEnabled: member, ControllerURL: controllerURL,
		MemberKeyConfigured: memberKey != "", MemberKey: memberKey, NodeID: nodeID, StatePath: statePath,
	}, nil
}

func parsePackaging(value string) (Packaging, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return PackagingNative, nil
	}
	value = strings.ReplaceAll(value, "-", "_")
	switch Packaging(value) {
	case PackagingNative, PackagingAllInOne, PackagingControlPlane:
		return Packaging(value), nil
	default:
		return "", invalid("deployment.packaging", "部署封装必须为 native、all_in_one 或 control_plane")
	}
}

func parseBool(field, value string, fallback bool) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, invalid(field, "开关值必须为 true 或 false")
	}
	return parsed, nil
}

func normalizeControllerURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", invalid("fleet.controllerUrl", "管理中心地址必须是无凭据和查询参数的 ws:// 或 wss:// 地址")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/agent"
	}
	if parsed.Path != "/agent" {
		return "", invalid("fleet.controllerUrl", "管理中心地址必须指向 /agent")
	}
	return parsed.String(), nil
}

func invalid(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}
