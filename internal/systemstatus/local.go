package systemstatus

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"dont/internal/hostresource"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

type LocalProvider struct {
	startedAt time.Time
	diskPath  string
	now       func() time.Time
}

func NewLocalProvider(diskPath string) *LocalProvider {
	diskPath = strings.TrimSpace(diskPath)
	if diskPath == "" {
		diskPath = "/"
		if runtime.GOOS == "windows" {
			diskPath = `C:\`
		}
	}
	return &LocalProvider{startedAt: time.Now().UTC(), diskPath: diskPath, now: time.Now}
}

func (p *LocalProvider) Status() Status {
	value := Status{ObservedAt: p.now().UTC(), Warnings: []string{}}
	p.collectHost(&value)
	p.collectCPU(&value)
	p.collectMemory(&value)
	p.collectDisk(&value)
	p.collectProcess(&value)
	p.collectRuntime(&value)
	return value
}

func (p *LocalProvider) collectHost(value *Status) {
	info, err := host.Info()
	if err != nil {
		value.Host.Error = err.Error()
		value.Warnings = append(value.Warnings, "无法读取主机信息")
		return
	}
	value.Host = HostStatus{Available: true, Hostname: info.Hostname, Platform: info.Platform, Version: info.PlatformVersion, Kernel: info.KernelVersion, Architecture: runtime.GOARCH, UptimeSeconds: info.Uptime}
}

func (p *LocalProvider) collectCPU(value *Status) {
	cores, coreErr := cpu.Counts(false)
	threads, threadErr := cpu.Counts(true)
	limits := hostresource.Detect()
	threads = hostresource.EffectiveCPUCount(threads, limits)
	usage, usageErr := cpu.Percent(100*time.Millisecond, true)
	if coreErr != nil || threadErr != nil || usageErr != nil {
		value.CPU.Error = firstError(coreErr, threadErr, usageErr)
		value.Warnings = append(value.Warnings, "无法完整读取 CPU 指标")
		return
	}
	model := ""
	if items, err := cpu.Info(); err == nil && len(items) > 0 {
		model = strings.TrimSpace(items[0].ModelName)
	}
	average := 0.0
	for index := range usage {
		usage[index] = percent(usage[index])
		average += usage[index]
	}
	if len(usage) > 0 {
		average /= float64(len(usage))
	}
	if cores > threads {
		cores = threads
	}
	value.CPU = CPUStatus{Available: true, Model: model, Cores: cores, Threads: threads, Usage: percent(average), CoreUsage: usage}
	if average, err := load.Avg(); err == nil {
		value.CPU.Load1, value.CPU.Load5, value.CPU.Load15, value.CPU.LoadSupport = average.Load1, average.Load5, average.Load15, true
	}
}

func (p *LocalProvider) collectMemory(value *Status) {
	info, err := mem.VirtualMemory()
	if err != nil {
		value.Memory.Error = err.Error()
		value.Warnings = append(value.Warnings, "无法读取内存指标")
		return
	}
	total, used, available := hostresource.EffectiveMemory(info.Total, info.Used, info.Available, hostresource.Detect())
	usage := 0.0
	if total > 0 {
		usage = float64(used) * 100 / float64(total)
	}
	value.Memory = MemoryStatus{Available: true, TotalBytes: total, UsedBytes: used, AvailableBytes: available, Usage: percent(usage)}
}

func (p *LocalProvider) collectDisk(value *Status) {
	info, err := disk.Usage(p.diskPath)
	if err != nil {
		value.Disk = DiskStatus{Path: p.diskPath, Error: err.Error()}
		value.Warnings = append(value.Warnings, "无法读取磁盘指标")
		return
	}
	value.Disk = DiskStatus{Available: true, Path: p.diskPath, TotalBytes: info.Total, UsedBytes: info.Used, AvailableBytes: info.Free, Usage: percent(info.UsedPercent)}
}

func (p *LocalProvider) collectProcess(value *Status) {
	value.Process = ProcessStatus{PID: os.Getpid(), UptimeSeconds: int64(p.now().Sub(p.startedAt).Seconds())}
	current, err := process.NewProcess(int32(value.Process.PID))
	if err != nil {
		value.Process.Error = err.Error()
		value.Warnings = append(value.Warnings, "无法读取管理进程指标")
		return
	}
	value.Process.Available = true
	if memory, memoryErr := current.MemoryInfo(); memoryErr == nil && memory != nil {
		value.Process.MemoryRSSBytes, value.Process.MemoryVMSBytes = memory.RSS, memory.VMS
	}
	if usage, usageErr := current.CPUPercent(); usageErr == nil {
		value.Process.CPUUsage = percent(usage)
	}
	if threads, threadsErr := current.NumThreads(); threadsErr == nil {
		value.Process.Threads = threads
	}
}

func (p *LocalProvider) collectRuntime(value *Status) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	pause := uint64(0)
	if stats.NumGC > 0 {
		pause = stats.PauseNs[(stats.NumGC-1)%uint32(len(stats.PauseNs))]
	}
	value.Runtime = RuntimeStatus{GoVersion: runtime.Version(), Goroutines: runtime.NumGoroutine(), HeapAllocBytes: stats.HeapAlloc, SystemBytes: stats.Sys, HeapObjects: stats.HeapObjects, LastGCPauseNano: pause, GCRuns: stats.NumGC}
}

func firstError(errors ...error) string {
	for _, err := range errors {
		if err != nil {
			return err.Error()
		}
	}
	return ""
}

func percent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func (p *LocalProvider) String() string { return fmt.Sprintf("local status provider (%s)", p.diskPath) }
