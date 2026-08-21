package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/internal/consoledispatch"
	"dont/internal/shards"
	"dont/shared"
)

// ContainerRuntimeHostConfig contains the host-only materialization settings
// used when the embedded executor creates a Shard container on first start.
// These values come from trusted deployment configuration, never from an API
// request.
type ContainerRuntimeHostConfig struct {
	Installation   RuntimeInstallation
	Image          string
	HostSavePath   string
	HostServerPath string
	HostUGCPath    string
	Timezone       string
}

// ContainerRuntimeHost exposes the trusted container Runtime implementation to
// an embedded local executor. Remote Agents and the control plane therefore use
// the same container discovery, console and CPU enforcement code.
type ContainerRuntimeHost struct {
	installation RuntimeInstallation
	control      *containerShardRuntime
	config       ContainerRuntimeHostConfig
	createMu     sync.Mutex
}

func NewContainerRuntimeHost(config ContainerRuntimeHostConfig) (*ContainerRuntimeHost, error) {
	return newContainerRuntimeHost(config, nil)
}

func newContainerRuntimeHost(config ContainerRuntimeHostConfig, cli containerCLI) (*ContainerRuntimeHost, error) {
	installation := config.Installation
	installation.Driver = "container"
	values, err := normalizeRuntimeInstallations([]RuntimeInstallation{installation})
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, errors.New("容器 Runtime 安装配置无效")
	}
	installation = values[0]
	config.Installation = installation
	config.Image = strings.TrimSpace(config.Image)
	config.HostSavePath = filepath.Clean(strings.TrimSpace(config.HostSavePath))
	config.HostServerPath = filepath.Clean(strings.TrimSpace(config.HostServerPath))
	config.HostUGCPath = filepath.Clean(strings.TrimSpace(config.HostUGCPath))
	config.Timezone = strings.TrimSpace(config.Timezone)
	if config.Image == "" || strings.ContainsAny(config.Image, "\x00\r\n") {
		return nil, errors.New("容器 Runtime 镜像配置无效")
	}
	if !trustedHostRuntimePath(config.HostSavePath) || !trustedHostRuntimePath(config.HostServerPath) || !trustedHostRuntimePath(config.HostUGCPath) ||
		pathsOverlap(config.HostSavePath, config.HostServerPath) || pathsOverlap(config.HostSavePath, config.HostUGCPath) ||
		pathsOverlap(config.HostServerPath, config.HostUGCPath) {
		return nil, errors.New("容器 Runtime 宿主数据路径无效或重叠")
	}
	if strings.ContainsAny(config.Timezone, "\x00\r\n") {
		return nil, errors.New("容器 Runtime 时区配置无效")
	}
	if cli == nil {
		cli = newExecContainerCLI(installation.ContainerEngine)
	}
	control, err := newContainerShardRuntime(installation, cli)
	if err != nil {
		return nil, err
	}
	return &ContainerRuntimeHost{installation: installation, control: control, config: config}, nil
}

func trustedHostRuntimePath(value string) bool {
	return trustedAbsolutePath(value) && value != string(filepath.Separator) && !strings.ContainsAny(value, "\x00\r\n:,")
}

func (h *ContainerRuntimeHost) IsRunning(ctx context.Context, cluster, shard string) (bool, error) {
	status, err := h.Status(ctx, cluster, shard)
	return status.State == shards.RuntimeRunning || status.State == shards.RuntimeStarting, err
}

func (h *ContainerRuntimeHost) Status(ctx context.Context, cluster, shard string) (shards.RuntimeStatus, error) {
	status, err := h.control.Status(ctx, cluster, shard)
	if errors.Is(err, errManagedContainerNotFound) {
		return shards.RuntimeStatus{State: shards.RuntimeStopped, Code: "CONTAINER_NOT_CREATED", Message: "首次启动时将自动创建世界容器"}, nil
	}
	return status, err
}

func (h *ContainerRuntimeHost) Start(ctx context.Context, cluster, shard string) error {
	if err := h.ensureContainer(ctx, cluster, shard); err != nil {
		return err
	}
	return h.control.Start(ctx, cluster, shard)
}

func (h *ContainerRuntimeHost) Stop(ctx context.Context, cluster, shard string) error {
	err := h.control.Stop(ctx, cluster, shard)
	if errors.Is(err, errManagedContainerNotFound) {
		return nil
	}
	return err
}

func (h *ContainerRuntimeHost) Cleanup(ctx context.Context, cluster, shard string) error {
	instance, err := h.control.find(ctx, cluster, shard)
	if errors.Is(err, errManagedContainerNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if instance.State == "running" || instance.State == "restarting" {
		return errors.New("分片容器仍在运行，请先停止世界")
	}
	if _, err := h.control.cli.Run(ctx, "rm", "--force", instance.ID); err != nil {
		return fmt.Errorf("清理分片容器: %w", err)
	}
	return nil
}

func (h *ContainerRuntimeHost) ensureContainer(ctx context.Context, cluster, shard string) error {
	h.createMu.Lock()
	defer h.createMu.Unlock()
	items, err := h.control.list(ctx, cluster, shard)
	if err != nil {
		return err
	}
	if len(items) == 1 {
		return nil
	}
	if len(items) > 1 {
		return errors.New("发现多个相同 Placement 的受管分片容器")
	}
	arguments := []string{
		"create",
		"--name", h.containerName(cluster, shard),
		"--label", "com.dst-admin.managed=true",
		"--label", "com.dst-admin.installation=" + h.installation.ID,
		"--label", "com.dst-admin.cluster=" + cluster,
		"--label", "com.dst-admin.shard=" + shard,
		"--network", "host",
		"--restart", "no",
		"--stop-timeout", "45",
		"--read-only",
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--pids-limit", "512",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777",
		"--tmpfs", "/run/dst-admin:rw,noexec,nosuid,nodev,size=16m,uid=10000,gid=10000,mode=0700",
		"--mount", "type=bind,src=" + h.config.HostSavePath + ",dst=/data",
		"--mount", "type=bind,src=" + h.config.HostServerPath + ",dst=/opt/dst/server,readonly",
		"--mount", "type=bind,src=" + h.config.HostUGCPath + ",dst=/workshop,readonly",
		"--env", "DST_CLUSTER=" + cluster,
		"--env", "DST_SHARD=" + shard,
		"--env", "DST_STORAGE_ROOT=/data",
		"--env", "DST_CONF_DIR=.",
		"--env", "DST_UGC_DIRECTORY=/workshop",
	}
	if h.config.Timezone != "" {
		arguments = append(arguments, "--env", "TZ="+h.config.Timezone)
	}
	arguments = append(arguments, h.config.Image)
	if _, err := h.control.cli.Run(ctx, arguments...); err != nil {
		return fmt.Errorf("创建分片容器: %w", err)
	}
	return nil
}

func (h *ContainerRuntimeHost) containerName(cluster, shard string) string {
	digest := sha256.Sum256([]byte(h.installation.ID + "\x00" + cluster + "\x00" + shard))
	return "dst-admin-shard-" + hex.EncodeToString(digest[:8])
}

func (h *ContainerRuntimeHost) Send(ctx context.Context, cluster, shard, command string) error {
	return h.control.Send(ctx, cluster, shard, command)
}

func (h *ContainerRuntimeHost) SendBackground(ctx context.Context, cluster, shard, coalesceKey, command string) error {
	return h.control.SendBackground(ctx, cluster, shard, coalesceKey, command)
}

func (h *ContainerRuntimeHost) ConsoleHealth(cluster, shard string) consoledispatch.Health {
	return h.control.ConsoleHealth(cluster, shard)
}

func (h *ContainerRuntimeHost) ContainerProcesses(ctx context.Context) ([]shared.ShardProcessReport, error) {
	return h.control.ContainerProcesses(ctx)
}

func (h *ContainerRuntimeHost) Prepare(ctx context.Context, installationID, cluster, shard string, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	return h.executeCPU(ctx, shared.RuntimeActionCPUPrepare, installationID, cluster, shard, request)
}

func (h *ContainerRuntimeHost) Apply(ctx context.Context, installationID, cluster, shard string, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	return h.executeCPU(ctx, shared.RuntimeActionCPUApply, installationID, cluster, shard, request)
}

func (h *ContainerRuntimeHost) Observe(ctx context.Context, installationID, cluster, shard string, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	return h.executeCPU(ctx, shared.RuntimeActionCPUObserve, installationID, cluster, shard, request)
}

func (h *ContainerRuntimeHost) executeCPU(ctx context.Context, action shared.RuntimeAction, installationID, cluster, shard string, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	if strings.TrimSpace(installationID) != h.installation.ID {
		return shared.RuntimeCPUResult{}, errors.New("容器 Runtime 安装标识不匹配")
	}
	if action == shared.RuntimeActionCPUPrepare {
		if err := h.ensureContainer(ctx, cluster, shard); err != nil {
			return shared.RuntimeCPUResult{}, err
		}
	}
	if action != shared.RuntimeActionCPUPrepare && request.Policy == shared.RuntimeCPUPolicyNone {
		items, err := h.control.list(ctx, cluster, shard)
		if err != nil {
			return shared.RuntimeCPUResult{}, err
		}
		if len(items) == 0 {
			return shared.RuntimeCPUResult{
				Policy: request.Policy, State: shared.RuntimeCPUStateReleased, RuntimeKind: "container", Enforced: true,
				ObservedAt: time.Now().UTC(),
			}, nil
		}
	}
	return h.control.ExecuteCPU(ctx, cluster, shard, action, request)
}
