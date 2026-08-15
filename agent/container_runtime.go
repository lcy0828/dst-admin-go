package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"dont/internal/consoledispatch"
	"dont/internal/shards"
	"dont/shared"
)

const maximumContainerCLIOutput = 256 * 1024

var managedContainerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

type containerCLI interface {
	Available() bool
	Run(context.Context, ...string) ([]byte, error)
}

type execContainerCLI struct{ binary string }

func newExecContainerCLI(name string) *execContainerCLI {
	binary, _ := exec.LookPath(strings.TrimSpace(name))
	return &execContainerCLI{binary: binary}
}

func (c *execContainerCLI) Available() bool { return c != nil && c.binary != "" }

func (c *execContainerCLI) Run(ctx context.Context, arguments ...string) ([]byte, error) {
	if !c.Available() {
		return nil, errors.New("容器 CLI 不可用")
	}
	output, err := exec.CommandContext(ctx, c.binary, arguments...).CombinedOutput()
	if len(output) > maximumContainerCLIOutput {
		output = append(output[:maximumContainerCLIOutput], []byte("\n[输出已截断]")...)
	}
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return output, errors.New(message)
	}
	return output, nil
}

type managedContainer struct {
	ID      string
	Name    string
	State   string
	Cluster string
	Shard   string
}

type containerShardRuntime struct {
	installation RuntimeInstallation
	cli          containerCLI
	dispatcher   *consoledispatch.Dispatcher
}

type containerInventoryProvider interface {
	ContainerProcesses(context.Context) ([]shared.ShardProcessReport, error)
}

func newContainerShardRuntime(installation RuntimeInstallation, cli containerCLI) (*containerShardRuntime, error) {
	if installation.Driver != "container" || cli == nil || !cli.Available() {
		return nil, errors.New("容器 Runtime 未正确配置或容器 CLI 不可用")
	}
	return &containerShardRuntime{installation: installation, cli: cli, dispatcher: consoledispatch.New()}, nil
}

func (c *containerShardRuntime) Status(ctx context.Context, cluster, shard string) (shards.RuntimeStatus, error) {
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		return shards.RuntimeStatus{State: shards.RuntimeUnknown}, err
	}
	switch instance.State {
	case "running":
		_, healthErr := c.cli.Run(ctx, "exec", instance.ID, "tmux", "-S", c.installation.ConsoleSocket, "has-session", "-t", "="+c.installation.ConsoleSession)
		if healthErr != nil {
			return shards.RuntimeStatus{State: shards.RuntimeStarting, Code: "CONSOLE_STARTING", Message: "容器已运行，等待 tmux 与 DST 会话就绪", SessionExists: true}, nil
		}
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	case "restarting":
		return shards.RuntimeStatus{State: shards.RuntimeStarting, Message: "容器正在启动", SessionExists: true}, nil
	case "created", "exited":
		return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
	default:
		return shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "CONTAINER_" + strings.ToUpper(instance.State), Message: "分片容器状态异常: " + instance.State, SessionExists: instance.State != "dead"}, nil
	}
}

func (c *containerShardRuntime) Start(ctx context.Context, cluster, shard string) error {
	key := c.shardKey(cluster, shard)
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		_ = c.dispatcher.Pause(context.Background(), key)
		return err
	}
	if instance.State == "running" || instance.State == "restarting" {
		if err := c.dispatcher.BindInstance(key, instance.ID); err != nil {
			return err
		}
		return c.dispatcher.Resume(key)
	}
	if _, err := c.cli.Run(ctx, "start", instance.ID); err != nil {
		_ = c.dispatcher.Pause(context.Background(), key)
		return fmt.Errorf("启动分片容器: %w", err)
	}
	if err := c.dispatcher.BindInstance(key, instance.ID); err != nil {
		return err
	}
	return c.dispatcher.Resume(key)
}

func (c *containerShardRuntime) Stop(ctx context.Context, cluster, shard string) error {
	key := c.shardKey(cluster, shard)
	if err := c.dispatcher.Pause(ctx, key); err != nil {
		return err
	}
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		_ = c.dispatcher.Resume(key)
		return err
	}
	if instance.State != "running" {
		return nil
	}
	if err := c.sendToInstance(ctx, instance.ID, "c_shutdown(true)"); err != nil {
		_ = c.dispatcher.Resume(key)
		return fmt.Errorf("向分片容器发送优雅停止命令: %w", err)
	}
	return nil
}

func (c *containerShardRuntime) Send(ctx context.Context, cluster, shard, command string) error {
	instance, err := c.find(ctx, cluster, shard)
	if err != nil {
		return err
	}
	if instance.State != "running" {
		return errors.New("分片容器未运行")
	}
	key := c.shardKey(cluster, shard)
	if err := c.dispatcher.BindInstance(key, instance.ID); err != nil {
		return err
	}
	writeAttempted := false
	err = c.dispatcher.Dispatch(ctx, key, consoledispatch.Request{InstanceID: instance.ID, Execute: func(sendContext context.Context) error {
		current, err := c.find(sendContext, cluster, shard)
		if err != nil {
			return err
		}
		if current.ID != instance.ID {
			return consoledispatch.ErrInstanceChanged
		}
		if current.State != "running" {
			return errors.New("分片容器未运行")
		}
		writeAttempted = true
		return c.sendToInstance(sendContext, current.ID, command)
	}})
	if err != nil && writeAttempted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.dispatcher.MarkInputDirty(key)
	}
	return err
}

func (c *containerShardRuntime) SendBackground(ctx context.Context, cluster, shard, coalesceKey, command string) error {
	key := c.shardKey(cluster, shard)
	instanceID := c.dispatcher.Health(key).InstanceID
	if instanceID == "" {
		instance, err := c.find(ctx, cluster, shard)
		if err != nil {
			return err
		}
		if instance.State != "running" {
			return errors.New("分片容器未运行")
		}
		instanceID = instance.ID
		if err := c.dispatcher.BindInstance(key, instanceID); err != nil {
			return err
		}
	}
	writeAttempted := false
	err := c.dispatcher.Dispatch(ctx, key, consoledispatch.Request{
		Class: consoledispatch.ClassBackground, CoalesceKey: coalesceKey, InstanceID: instanceID,
		Execute: func(sendContext context.Context) error {
			current, err := c.find(sendContext, cluster, shard)
			if err != nil {
				return err
			}
			if current.ID != instanceID {
				return consoledispatch.ErrInstanceChanged
			}
			if current.State != "running" {
				return errors.New("分片容器未运行")
			}
			writeAttempted = true
			return c.sendToInstance(sendContext, current.ID, command)
		},
	})
	if err != nil && writeAttempted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.dispatcher.MarkInputDirty(key)
	}
	return err
}

func (c *containerShardRuntime) ConsoleHealth(cluster, shard string) consoledispatch.Health {
	return c.dispatcher.Health(c.shardKey(cluster, shard))
}

func (c *containerShardRuntime) ContainerProcesses(ctx context.Context) ([]shared.ShardProcessReport, error) {
	instances, err := c.list(ctx, "", "")
	if err != nil {
		return nil, err
	}
	result := make([]shared.ShardProcessReport, 0, len(instances))
	for _, instance := range instances {
		if instance.State != "running" && instance.State != "restarting" {
			continue
		}
		result = append(result, shared.ShardProcessReport{
			PID: containerPseudoPID(instance.ID), RuntimeKind: "container", InstanceID: instance.ID,
			Executable: "container:" + instance.Name, Cluster: instance.Cluster, Shard: instance.Shard,
		})
	}
	return result, nil
}

func (c *containerShardRuntime) sendToInstance(ctx context.Context, id, command string) error {
	_, err := c.cli.Run(ctx, "exec", id, "tmux", "-S", c.installation.ConsoleSocket,
		"send-keys", "-t", "="+c.installation.ConsoleSession+":0.0", "-l", "--", command,
		";", "send-keys", "-t", "="+c.installation.ConsoleSession+":0.0", "Enter")
	return err
}

func (c *containerShardRuntime) find(ctx context.Context, cluster, shard string) (managedContainer, error) {
	items, err := c.list(ctx, cluster, shard)
	if err != nil {
		return managedContainer{}, err
	}
	if len(items) == 0 {
		return managedContainer{}, errors.New("未找到受管分片容器")
	}
	if len(items) != 1 {
		return managedContainer{}, errors.New("发现多个相同 Placement 的受管分片容器")
	}
	return items[0], nil
}

func (c *containerShardRuntime) list(ctx context.Context, cluster, shard string) ([]managedContainer, error) {
	arguments := []string{"ps", "-a", "--no-trunc", "--filter", "label=com.dst-admin.managed=true", "--filter", "label=com.dst-admin.installation=" + c.installation.ID}
	if cluster != "" {
		arguments = append(arguments, "--filter", "label=com.dst-admin.cluster="+cluster)
	}
	if shard != "" {
		arguments = append(arguments, "--filter", "label=com.dst-admin.shard="+shard)
	}
	arguments = append(arguments, "--format", "{{json .}}")
	output, err := c.cli.Run(ctx, arguments...)
	if err != nil {
		return nil, err
	}
	if len(output) > maximumContainerCLIOutput {
		return nil, errors.New("容器 CLI 返回的清单过大")
	}
	items := make([]managedContainer, 0)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var raw map[string]string
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, errors.New("容器 CLI 返回了无效清单")
		}
		labels := parseContainerLabels(raw["Labels"])
		id := strings.ToLower(strings.TrimSpace(raw["ID"]))
		if !managedContainerID.MatchString(id) || labels["com.dst-admin.managed"] != "true" || labels["com.dst-admin.installation"] != c.installation.ID {
			return nil, errors.New("容器清单包含不受信目标")
		}
		item := managedContainer{ID: id, Name: strings.TrimSpace(raw["Names"]), State: strings.ToLower(strings.TrimSpace(raw["State"])), Cluster: labels["com.dst-admin.cluster"], Shard: labels["com.dst-admin.shard"]}
		if item.Cluster == "" || item.Shard == "" {
			return nil, errors.New("受管容器缺少 Cluster 或 Shard 标签")
		}
		if cluster != "" && item.Cluster != cluster || shard != "" && item.Shard != shard {
			return nil, errors.New("容器清单返回了不匹配请求的 Placement")
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (c *containerShardRuntime) shardKey(cluster, shard string) string {
	return c.installation.ID + "\x00container\x00" + cluster + "\x00" + shard
}

func parseContainerLabels(value string) map[string]string {
	result := make(map[string]string)
	for _, item := range strings.Split(value, ",") {
		key, raw, ok := strings.Cut(item, "=")
		if ok {
			result[strings.TrimSpace(key)] = strings.TrimSpace(raw)
		}
	}
	return result
}

func containerPseudoPID(id string) int32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	return int32(hash.Sum32()%0x7ffffffe) + 1
}
