package runtimeinventory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dont/shared"

	"github.com/go-ini/ini"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	maximumInventoryRooms  = 256
	maximumInventoryShards = 64
)

// Collect reads one machine's DST installation, save tree, resources, and
// running shard processes without changing local state.
func Collect(ctx context.Context, request shared.RuntimeInventoryRequest) (shared.RuntimeInventoryReport, error) {
	request.InstallationID = strings.TrimSpace(request.InstallationID)
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	request.SavePath = filepath.Clean(strings.TrimSpace(request.SavePath))
	request.ServerPath = filepath.Clean(strings.TrimSpace(request.ServerPath))
	request.ServerMode = strings.TrimSpace(request.ServerMode)
	if request.InstallationID == "" || request.SavePath == "." || request.ServerPath == "." ||
		!filepath.IsAbs(request.SavePath) || !filepath.IsAbs(request.ServerPath) ||
		strings.ContainsAny(request.InstallationID+request.DisplayName+request.SavePath+request.ServerPath, "\x00\r\n") {
		return shared.RuntimeInventoryReport{}, errors.New("DST 运行时清单路径或安装标识无效")
	}
	cpuInfo, memoryInfo := HostResources()
	report := shared.RuntimeInventoryReport{
		ProtocolVersion: shared.RuntimeInventoryProtocolVersion,
		ObservedAt:      time.Now().UTC(),
		CPU:             cpuInfo,
		Memory:          memoryInfo,
		Installation: shared.RuntimeInstallationReport{
			ID: request.InstallationID, DisplayName: request.DisplayName,
			SavePath: request.SavePath, ServerPath: request.ServerPath, ServerMode: request.ServerMode,
			SavePathOK: directoryExists(request.SavePath), ServerPathOK: directoryExists(request.ServerPath),
		},
		Rooms: []shared.RoomInventoryReport{}, Processes: []shared.ShardProcessReport{}, Warnings: []string{},
	}
	if !report.Installation.SavePathOK {
		report.Warnings = append(report.Warnings, "存档根目录不存在或不可访问")
	} else {
		rooms, warnings, err := scanRooms(request.SavePath)
		if err != nil {
			return shared.RuntimeInventoryReport{}, err
		}
		report.Rooms = rooms
		report.Warnings = append(report.Warnings, warnings...)
	}
	if !report.Installation.ServerPathOK {
		report.Warnings = append(report.Warnings, "DST 服务端目录不存在或不可访问")
	}
	report.Processes = CollectDSTProcesses(ctx)
	return report, nil
}

func HostResources() (shared.CPUInventory, shared.MemoryInventory) {
	logical, err := cpu.Counts(true)
	if err != nil || logical < 1 {
		logical = 1
	}
	physical, err := cpu.Counts(false)
	source := "gopsutil"
	estimated := false
	if err != nil || physical < 1 {
		physical = logical / 2
		if physical < 1 {
			physical = 1
		}
		source = "logical_estimate"
		estimated = true
	}
	if physical > logical {
		physical = logical
	}
	cpuInfo := shared.CPUInventory{
		LogicalProcessors: logical, PhysicalCores: physical,
		PhysicalCoreSource: source, PhysicalCoreEstimated: estimated,
	}
	memoryInfo := shared.MemoryInventory{}
	if value, memoryErr := mem.VirtualMemory(); memoryErr == nil && value != nil {
		memoryInfo.TotalBytes = value.Total
		memoryInfo.UsedBytes = value.Used
		memoryInfo.AvailableBytes = value.Available
	}
	return cpuInfo, memoryInfo
}

func scanRooms(saveRoot string) ([]shared.RoomInventoryReport, []string, error) {
	entries, err := os.ReadDir(saveRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 DST 存档根目录失败: %w", err)
	}
	rooms := make([]shared.RoomInventoryReport, 0)
	warnings := make([]string, 0)
	for _, entry := range entries {
		if len(rooms) >= maximumInventoryRooms {
			warnings = append(warnings, fmt.Sprintf("房间数量超过 %d，清单已截断", maximumInventoryRooms))
			break
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		roomPath := filepath.Join(saveRoot, entry.Name())
		clusterPath := filepath.Join(roomPath, "cluster.ini")
		clusterConfig, err := ini.Load(clusterPath)
		if err != nil {
			continue
		}
		room := shared.RoomInventoryReport{
			Directory:     entry.Name(),
			Name:          strings.TrimSpace(clusterConfig.Section("NETWORK").Key("cluster_name").String()),
			ConfigPath:    clusterPath,
			MasterPort:    clusterConfig.Section("SHARD").Key("master_port").MustInt(0),
			ClusterKeySet: strings.TrimSpace(clusterConfig.Section("SHARD").Key("cluster_key").String()) != "",
			Shards:        []shared.ShardInventoryReport{},
		}
		if room.Name == "" {
			room.Name = entry.Name()
		}
		shardEntries, readErr := os.ReadDir(roomPath)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("无法读取房间 %s 的分片目录", entry.Name()))
			rooms = append(rooms, room)
			continue
		}
		for _, shardEntry := range shardEntries {
			if len(room.Shards) >= maximumInventoryShards {
				warnings = append(warnings, fmt.Sprintf("房间 %s 的分片数量超过 %d，清单已截断", entry.Name(), maximumInventoryShards))
				break
			}
			if !shardEntry.IsDir() || shardEntry.Type()&os.ModeSymlink != 0 {
				continue
			}
			serverPath := filepath.Join(roomPath, shardEntry.Name(), "server.ini")
			serverConfig, loadErr := ini.Load(serverPath)
			if loadErr != nil {
				continue
			}
			isMaster := serverConfig.Section("SHARD").Key("is_master").MustBool(false)
			role := "secondary"
			if isMaster {
				role = "master"
			}
			name := strings.TrimSpace(serverConfig.Section("SHARD").Key("name").String())
			if name == "" {
				name = shardEntry.Name()
			}
			room.Shards = append(room.Shards, shared.ShardInventoryReport{
				Directory: shardEntry.Name(), Name: name,
				ID: serverConfig.Section("SHARD").Key("id").MustInt(0), Role: role, ConfigPath: serverPath,
				ServerPort:         serverConfig.Section("NETWORK").Key("server_port").MustInt(0),
				MasterServerPort:   serverConfig.Section("STEAM").Key("master_server_port").MustInt(0),
				AuthenticationPort: serverConfig.Section("STEAM").Key("authentication_port").MustInt(0),
			})
		}
		sort.Slice(room.Shards, func(i, j int) bool { return room.Shards[i].Directory < room.Shards[j].Directory })
		rooms = append(rooms, room)
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].Directory < rooms[j].Directory })
	return rooms, warnings, nil
}

func CollectDSTProcesses(ctx context.Context) []shared.ShardProcessReport {
	values, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return []shared.ShardProcessReport{}
	}
	items := make([]shared.ShardProcessReport, 0)
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			break
		}
		name, _ := value.NameWithContext(ctx)
		arguments, _ := value.CmdlineSliceWithContext(ctx)
		if !IsDSTServerProcess(name, arguments) {
			continue
		}
		item := ShardProcessFromArguments(value.Pid, name, arguments)
		if executable, executableErr := value.ExeWithContext(ctx); executableErr == nil && executable != "" {
			item.Executable = executable
		}
		if created, createdErr := value.CreateTimeWithContext(ctx); createdErr == nil && created > 0 {
			startedAt := time.UnixMilli(created).UTC()
			item.StartedAt = &startedAt
		}
		if percent, percentErr := value.CPUPercentWithContext(ctx); percentErr == nil && percent >= 0 {
			item.CPUPercent = percent
		}
		if memoryInfo, memoryErr := value.MemoryInfoWithContext(ctx); memoryErr == nil && memoryInfo != nil {
			item.RSSBytes = memoryInfo.RSS
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].PID < items[j].PID })
	return items
}

func IsDSTServerProcess(name string, arguments []string) bool {
	candidates := append([]string{name}, arguments...)
	for _, candidate := range candidates {
		base := strings.ToLower(filepath.Base(strings.TrimSpace(candidate)))
		if strings.Contains(base, "dontstarve_dedicated_server") {
			return true
		}
	}
	return false
}

func ShardProcessFromArguments(pid int32, name string, arguments []string) shared.ShardProcessReport {
	return shared.ShardProcessReport{
		PID: pid, Executable: name,
		Cluster:         commandFlag(arguments, "-cluster"),
		Shard:           commandFlag(arguments, "-shard"),
		StorageRoot:     commandFlag(arguments, "-persistent_storage_root"),
		ConfigDirectory: commandFlag(arguments, "-conf_dir"),
	}
}

func commandFlag(arguments []string, flag string) string {
	for index, argument := range arguments {
		if argument == flag && index+1 < len(arguments) {
			return strings.TrimSpace(arguments[index+1])
		}
		if strings.HasPrefix(argument, flag+"=") {
			return strings.TrimSpace(strings.TrimPrefix(argument, flag+"="))
		}
	}
	return ""
}

func directoryExists(path string) bool {
	value, err := os.Stat(path)
	return err == nil && value.IsDir()
}
