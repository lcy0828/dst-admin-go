package status

import (
	"github.com/gin-gonic/gin"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"net/http"
	"runtime"
	"strconv"
	"time"
)

// SystemInfo 系统信息结构体
type SystemInfo struct {
	// CPU信息
	CpuModel      string  `json:"cpu_model"`      // CPU型号
	CpuMhz        float64 `json:"cpu_mhz"`        // CPU频率
	CpuCores      int     `json:"cpu_cores"`      // CPU物理核心数
	CpuThreads    int     `json:"cpu_threads"`    // CPU逻辑核心数
	CpuUsage      float64 `json:"cpu_usage"`      // CPU使用率(%)
	CpuLoad1      float64 `json:"cpu_load1"`      // 1分钟平均负载
	CpuLoad5      float64 `json:"cpu_load5"`      // 5分钟平均负载
	CpuLoad15     float64 `json:"cpu_load15"`     // 15分钟平均负载
	
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
	
	// 时间信息
	CurrentTime   string  `json:"current_time"`   // 当前系统时间
}

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
	diskInfo, _ := disk.Usage("/")
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
