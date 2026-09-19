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

	"dont/internal/hostresource"
	"dont/internal/worldidentity"
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
	processes, processErr := ProbeDSTProcesses(ctx)
	if processErr != nil {
		report.Warnings = append(report.Warnings, "无法读取 DST 进程清单: "+processErr.Error())
	} else {
		report.Processes = installationProcesses(processes, report.Rooms, request.SavePath, request.ServerPath)
	}
	return report, nil
}

func installationProcesses(values []shared.ShardProcessReport, rooms []shared.RoomInventoryReport, savePath, serverPath string) []shared.ShardProcessReport {
	identities := make(map[string]bool)
	for _, room := range rooms {
		for _, shard := range room.Shards {
			identities[strings.ToLower(strings.TrimSpace(room.Directory))+"\x00"+strings.ToLower(strings.TrimSpace(shard.Directory))] = true
		}
	}
	result := make([]shared.ShardProcessReport, 0, len(values))
	for _, value := range values {
		matches := false
		if root := strings.TrimSpace(value.StorageRoot); root != "" {
			processSavePath := root
			if configDirectory := strings.TrimSpace(value.ConfigDirectory); configDirectory != "" {
				processSavePath = filepath.Join(root, configDirectory)
			}
			matches = sameOrDescendantPath(processSavePath, savePath)
		} else if executable := strings.TrimSpace(value.Executable); filepath.IsAbs(executable) {
			matches = sameOrDescendantPath(executable, serverPath)
		} else {
			identity := strings.ToLower(strings.TrimSpace(value.Cluster)) + "\x00" + strings.ToLower(strings.TrimSpace(value.Shard))
			matches = identities[identity]
		}
		if matches {
			result = append(result, value)
		}
	}
	return result
}

func sameOrDescendantPath(candidate, parent string) bool {
	candidate, parent = filepath.Clean(strings.TrimSpace(candidate)), filepath.Clean(strings.TrimSpace(parent))
	if candidate == "." || parent == "." {
		return false
	}
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func HostResources() (shared.CPUInventory, shared.MemoryInventory) {
	logical, err := cpu.Counts(true)
	if err != nil || logical < 1 {
		logical = 1
	}
	hostLogical := logical
	limits := hostresource.Detect()
	logical = hostresource.EffectiveCPUCount(hostLogical, limits)
	constrainedCPU := logical < hostLogical
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
	if constrainedCPU {
		physical = logical
		source = "cgroup_limit"
		estimated = true
	} else if physical > logical {
		physical = logical
	}
	cpuInfo := shared.CPUInventory{
		LogicalProcessors: logical, PhysicalCores: physical,
		PhysicalCoreSource: source, PhysicalCoreEstimated: estimated,
	}
	if !constrainedCPU {
		if threads, topologyOK := cpuTopology(logical); topologyOK {
			cpuInfo.TopologyAvailable = true
			cpuInfo.Threads = threads
			groups := make(map[string]int, len(threads))
			for _, thread := range threads {
				key := thread.PackageID + "\x00" + thread.CoreID
				groups[key]++
				if groups[key] > 1 {
					cpuInfo.SMTDetected = true
				}
			}
		}
	}
	memoryInfo := shared.MemoryInventory{}
	if value, memoryErr := mem.VirtualMemory(); memoryErr == nil && value != nil {
		memoryInfo.TotalBytes, memoryInfo.UsedBytes, memoryInfo.AvailableBytes = hostresource.EffectiveMemory(
			value.Total, value.Used, value.Available, limits,
		)
	}
	return cpuInfo, memoryInfo
}

func cpuTopology(logical int) ([]shared.CPUThreadInventory, bool) {
	values, err := cpu.Info()
	if err != nil || len(values) == 0 {
		return nil, false
	}
	seen := make(map[int]bool, len(values))
	threads := make([]shared.CPUThreadInventory, 0, len(values))
	for _, value := range values {
		logicalID := int(value.CPU)
		packageID := strings.TrimSpace(value.PhysicalID)
		coreID := strings.TrimSpace(value.CoreID)
		if logicalID < 0 || logicalID >= logical || packageID == "" || coreID == "" || seen[logicalID] {
			continue
		}
		seen[logicalID] = true
		threads = append(threads, shared.CPUThreadInventory{LogicalID: logicalID, PackageID: packageID, CoreID: coreID})
	}
	if len(threads) != logical {
		return nil, false
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].LogicalID < threads[j].LogicalID })
	return threads, true
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
			BindIP:        strings.TrimSpace(clusterConfig.Section("SHARD").Key("bind_ip").String()),
			MasterIP:      strings.TrimSpace(clusterConfig.Section("SHARD").Key("master_ip").String()),
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
				ID: serverConfig.Section("SHARD").Key("id").MustInt(0), Role: role,
				Type: string(worldidentity.ResolveType(filepath.Join(roomPath, shardEntry.Name()), shardEntry.Name())), ConfigPath: serverPath,
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
	items, err := ProbeDSTProcesses(ctx)
	if err != nil {
		return []shared.ShardProcessReport{}
	}
	return items
}

// ProbeDSTProcesses keeps failures visible for lifecycle preflight callers.
// Inventory/reporting callers may still use CollectDSTProcesses when an empty
// best-effort result is preferable to failing the whole report.
func ProbeDSTProcesses(ctx context.Context) ([]shared.ShardProcessReport, error) {
	values, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]shared.ShardProcessReport, 0)
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
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
	return items, nil
}

func IsDSTServerProcess(name string, arguments []string) bool {
	candidates := []string{name}
	if len(arguments) > 0 {
		candidates = append(candidates, arguments[0])
	}
	for _, candidate := range candidates {
		if IsDSTExecutable(candidate) {
			return true
		}
	}
	return false
}

// IsDSTExecutable distinguishes the actual game process from supervisors such
// as tmux whose command line embeds the full DST launch command.
func IsDSTExecutable(value string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(value)))
	return strings.Contains(base, "dontstarve_dedicated_server")
}

func ShardProcessFromArguments(pid int32, name string, arguments []string) shared.ShardProcessReport {
	mode := shared.RuntimePerformanceModeGame
	switch value := commandFlag(arguments, "-lua_vm_type"); value {
	case "", "game":
	case "jit":
		mode = shared.RuntimePerformanceModeLuaJIT
	case "jit_gen":
		mode = shared.RuntimePerformanceModeArenaGC
	default:
		mode = "unknown"
	}
	return shared.ShardProcessReport{
		PID: pid, Executable: name, RuntimeMode: mode,
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
