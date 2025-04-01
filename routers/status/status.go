package status

import (
	"bufio"
	"bytes"
	"github.com/gin-gonic/gin"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// SystemInfo 系统信息结构体
type SystemInfo struct {
	// CPU信息
	CpuModel      string    `json:"cpu_model"`      // CPU型号
	CpuMhz        float64   `json:"cpu_mhz"`        // CPU频率
	CpuCores      int       `json:"cpu_cores"`      // CPU物理核心数
	CpuThreads    int       `json:"cpu_threads"`    // CPU逻辑核心数
	CpuUsage      float64   `json:"cpu_usage"`      // CPU使用率(%)
	CpuCoreUsage  []float64 `json:"cpu_core_usage"` // 每个CPU核心的使用率(%)
	CpuLoad1      float64   `json:"cpu_load1"`      // 1分钟平均负载
	CpuLoad5      float64   `json:"cpu_load5"`      // 5分钟平均负载
	CpuLoad15     float64   `json:"cpu_load15"`     // 15分钟平均负载
	
	// 内存信息
	TotalMemory   uint64  `json:"total_memory"`   // 总内存(MB)
	UsedMemory    uint64  `json:"used_memory"`    // 已用内存(MB)
	FreeMemory    uint64  `json:"free_memory"`    // 空闲内存(MB)
	MemoryUsage   float64 `json:"memory_usage"`   // 内存使用率(%)
	
	// 磁盘信息
	TotalDisk     uint64  `json:"total_disk"`     // 总磁盘空间(GB)
	UsedDisk      uint64  `json:"used_disk"`      // 已用磁盘空间(GB)
	FreeDisk      uint64  `json:"free_disk"`      // 空闲磁盘空间(GB)
	DiskUsage     float64 `json:"disk_usage"`     // 磁盘使用率(%)
	
	// 系统信息
	OsInfo        string  `json:"os_info"`        // 操作系统信息
	Hostname      string  `json:"hostname"`       // 主机名
	Uptime        uint64  `json:"uptime"`         // 系统运行时间(秒)
	UptimeFormatted string `json:"uptime_formatted"` // 格式化的运行时间
	
	// Go运行时信息
	GoVersion     string  `json:"go_version"`     // Go版本
	GoRoutines    int     `json:"go_routines"`    // 当前goroutine数量
	
	// 程序自身信息
	ProcessID        int       `json:"process_id"`           // 进程ID
	ProcessUptime    uint64    `json:"process_uptime"`       // 程序运行时间(秒)
	ProcessUptimeFmt string    `json:"process_uptime_fmt"`   // 格式化的程序运行时间
	ProcessMemoryRSS uint64    `json:"process_memory_rss"`   // 程序物理内存使用(MB)
	ProcessMemoryVMS uint64    `json:"process_memory_vms"`   // 程序虚拟内存使用(MB)
	ProcessCPUUsage  float64   `json:"process_cpu_usage"`    // 程序CPU使用率(%)
	ProcessThreads   int32     `json:"process_threads"`      // 程序线程数
	
	// Go内存信息
	GoMemoryAlloc    uint64    `json:"go_memory_alloc"`      // Go堆分配内存(MB)
	GoMemorySys      uint64    `json:"go_memory_sys"`        // Go系统获取的内存(MB)
	GoMemoryHeapSys  uint64    `json:"go_memory_heap_sys"`   // Go堆系统获取的内存(MB)
	GoMemoryHeapObjects uint64 `json:"go_memory_heap_objs"`  // Go堆中的对象数量
	GoGCPause        uint64    `json:"go_gc_pause"`          // 最后一次GC暂停时间(ns)
	GoGCRuns         uint32    `json:"go_gc_runs"`           // GC运行次数
	
	// 时间信息
	CurrentTime   string  `json:"current_time"`   // 当前系统时间
	StartTime     string  `json:"start_time"`     // 程序启动时间
}

// DockerContainer Docker容器信息结构体
type DockerContainer struct {
	ContainerId   string `json:"container_id"`   // 容器ID
	Image         string `json:"image"`          // 镜像名称
	Command       string `json:"command"`        // 运行命令
	Created       string `json:"created"`        // 创建时间
	Status        string `json:"status"`         // 容器状态
	Ports         string `json:"ports"`          // 端口映射
	Names         string `json:"names"`          // 容器名称
	Running       bool   `json:"running"`        // 是否运行中
}

// DockerContainersResponse Docker容器列表响应
type DockerContainersResponse struct {
	Status int              `json:"status"`
	Msg    string           `json:"msg"`
	Data   []DockerContainer `json:"data"`
}

// 程序启动时间
var (
	startTime = time.Now()
)

// Cpuinfo 获取系统信息接口
func Cpuinfo() gin.HandlerFunc {
	return func(c *gin.Context) {
		systemInfo := getSystemInfo()
		c.JSON(http.StatusOK, gin.H{
			"status": 200,
			"msg":    "获取系统信息成功",
			"data":   systemInfo,
		})
	}
}

// getSystemInfo 收集系统信息
func getSystemInfo() SystemInfo {
	var info SystemInfo
	
	// 获取CPU信息
	cpuInfo, _ := cpu.Info()
	if len(cpuInfo) > 0 {
		info.CpuModel = cpuInfo[0].ModelName
		info.CpuMhz = cpuInfo[0].Mhz
	}
	
	// 获取CPU核心数
	info.CpuCores, _ = cpu.Counts(false)
	info.CpuThreads, _ = cpu.Counts(true)
	
	// 获取CPU使用率(过去1秒)
	cpuPercent, _ := cpu.Percent(time.Second, false)
	if len(cpuPercent) > 0 {
		info.CpuUsage = cpuPercent[0]
	}
	
	// 获取每个CPU核心的使用率
	perCpuPercent, _ := cpu.Percent(time.Second, true)
	info.CpuCoreUsage = perCpuPercent
	
	// 获取系统负载
	loadInfo, _ := load.Avg()
	info.CpuLoad1 = loadInfo.Load1
	info.CpuLoad5 = loadInfo.Load5
	info.CpuLoad15 = loadInfo.Load15
	
	// 获取内存信息
	memInfo, _ := mem.VirtualMemory()
	info.TotalMemory = memInfo.Total / 1024 / 1024  // 转为MB
	info.UsedMemory = memInfo.Used / 1024 / 1024
	info.FreeMemory = memInfo.Free / 1024 / 1024
	info.MemoryUsage = memInfo.UsedPercent
	
	// 获取磁盘信息
	rootPath := "/"
	if runtime.GOOS == "windows" {
		// Windows 系统获取 C 盘信息
		rootPath = "C:\\"
	}
	diskInfo, _ := disk.Usage(rootPath)
	info.TotalDisk = diskInfo.Total / 1024 / 1024 / 1024  // 转为GB
	info.UsedDisk = diskInfo.Used / 1024 / 1024 / 1024
	info.FreeDisk = diskInfo.Free / 1024 / 1024 / 1024
	info.DiskUsage = diskInfo.UsedPercent
	
	// 获取系统信息
	hostInfo, _ := host.Info()
	info.OsInfo = hostInfo.Platform + " " + hostInfo.PlatformVersion
	info.Hostname = hostInfo.Hostname
	info.Uptime = hostInfo.Uptime
	info.UptimeFormatted = formatUptime(hostInfo.Uptime)
	
	// 获取Go运行时信息
	info.GoVersion = runtime.Version()
	info.GoRoutines = runtime.NumGoroutine()
	
	// 获取当前时间
	info.CurrentTime = time.Now().Format("2006-01-02 15:04:05")
	
	// 获取程序自身信息
	info.ProcessID = os.Getpid()
	
	// 计算程序运行时间
	processUptime := time.Since(startTime).Seconds()
	info.ProcessUptime = uint64(processUptime)
	info.ProcessUptimeFmt = formatUptime(info.ProcessUptime)
	info.StartTime = startTime.Format("2006-01-02 15:04:05")
	
	// 获取当前进程信息
	proc, err := process.NewProcess(int32(info.ProcessID))
	if err == nil {
		// 获取进程内存信息
		if memInfo, err := proc.MemoryInfo(); err == nil && memInfo != nil {
			info.ProcessMemoryRSS = memInfo.RSS / 1024 / 1024  // 转为MB
			info.ProcessMemoryVMS = memInfo.VMS / 1024 / 1024  // 转为MB
		}
		
		// 获取进程CPU使用率
		if cpuPercent, err := proc.CPUPercent(); err == nil {
			info.ProcessCPUUsage = cpuPercent
		}
		
		// 获取线程数
		if numThreads, err := proc.NumThreads(); err == nil {
			info.ProcessThreads = numThreads
		}
	}
	
	// 获取Go内存统计信息
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	
	info.GoMemoryAlloc = memStats.Alloc / 1024 / 1024       // 转为MB
	info.GoMemorySys = memStats.Sys / 1024 / 1024           // 转为MB
	info.GoMemoryHeapSys = memStats.HeapSys / 1024 / 1024   // 转为MB
	info.GoMemoryHeapObjects = memStats.HeapObjects
	
	// GC信息
	info.GoGCPause = memStats.PauseNs[(memStats.NumGC+255)%256]  // 最近的GC暂停时间
	info.GoGCRuns = memStats.NumGC
	
	// 手动触发一次GC(可选)
	// debug.FreeOSMemory()
	
	// 获取最近GC统计信息
	gcStats := debug.GCStats{}
	debug.ReadGCStats(&gcStats)
	
	return info
}

// formatUptime 格式化运行时间
func formatUptime(uptime uint64) string {
	days := uptime / (60 * 60 * 24)
	hours := (uptime % (60 * 60 * 24)) / (60 * 60)
	minutes := (uptime % (60 * 60)) / 60
	seconds := uptime % 60
	
	return strconv.FormatUint(days, 10) + "天" + 
		strconv.FormatUint(hours, 10) + "小时" + 
		strconv.FormatUint(minutes, 10) + "分" + 
		strconv.FormatUint(seconds, 10) + "秒"
}

// GetDstDockerContainers 获取DST相关Docker容器列表
func GetDstDockerContainers() gin.HandlerFunc {
	return func(c *gin.Context) {
		containers, err := getDstContainers()
		if err != nil {
			c.JSON(http.StatusOK, DockerContainersResponse{
				Status: 500,
				Msg:    "获取Docker容器列表失败: " + err.Error(),
				Data:   []DockerContainer{},
			})
			return
		}
		
		c.JSON(http.StatusOK, DockerContainersResponse{
			Status: 200,
			Msg:    "获取Docker容器列表成功",
			Data:   containers,
		})
	}
}

// getDstContainers 获取DST相关的Docker容器列表
func getDstContainers() ([]DockerContainer, error) {
	// 执行docker ps -a命令
	cmd := exec.Command("docker", "ps", "-a", "--format", "{{.ID}}|{{.Image}}|{{.Command}}|{{.CreatedAt}}|{{.Status}}|{{.Ports}}|{{.Names}}")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	
	// 解析输出结果
	var containers []DockerContainer
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		line := scanner.Text()
		// 只保留包含dstserver的行
		if !strings.Contains(strings.ToLower(line), "dstserver") {
			continue
		}
		
		// 解析行内容
		parts := strings.Split(line, "|")
		if len(parts) < 7 {
			continue
		}
		
		// 判断容器是否正在运行
		isRunning := strings.Contains(strings.ToLower(parts[4]), "up")
		
		containers = append(containers, DockerContainer{
			ContainerId: parts[0],
			Image:       parts[1],
			Command:     parts[2],
			Created:     parts[3],
			Status:      parts[4],
			Ports:       parts[5],
			Names:       parts[6],
			Running:     isRunning,
		})
	}
	
	return containers, nil
}
