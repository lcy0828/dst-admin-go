package systemstatus

import "time"

type MemoryProvider struct{ now func() time.Time }

func NewMemoryProvider() *MemoryProvider { return &MemoryProvider{now: time.Now} }

func (p *MemoryProvider) Status() Status {
	return Status{
		ObservedAt: p.now().UTC(),
		Host:       HostStatus{Available: true, Hostname: "林火主机", Platform: "linux", Version: "test", Kernel: "6.8.0", Architecture: "amd64", UptimeSeconds: 172800},
		CPU:        CPUStatus{Available: true, UsageAvailable: true, Model: "Test CPU", Cores: 4, Threads: 8, Usage: 37.5, CoreUsage: []float64{20, 35, 45, 50}, Load1: 0.8, Load5: 0.6, Load15: 0.4, LoadSupport: true},
		Memory:     MemoryStatus{Available: true, TotalBytes: 16 * 1024 * 1024 * 1024, UsedBytes: 6 * 1024 * 1024 * 1024, AvailableBytes: 10 * 1024 * 1024 * 1024, Usage: 37.5},
		Disk:       DiskStatus{Available: true, Path: "/", TotalBytes: 256 * 1024 * 1024 * 1024, UsedBytes: 128 * 1024 * 1024 * 1024, AvailableBytes: 128 * 1024 * 1024 * 1024, Usage: 50},
		Process:    ProcessStatus{Available: true, PID: 4321, UptimeSeconds: 3600, CPUUsage: 2.5, MemoryRSSBytes: 96 * 1024 * 1024, MemoryVMSBytes: 512 * 1024 * 1024, Threads: 12},
		Runtime:    RuntimeStatus{GoVersion: "go1.25-test", Goroutines: 42, HeapAllocBytes: 32 * 1024 * 1024, SystemBytes: 64 * 1024 * 1024, HeapObjects: 12000, LastGCPauseNano: 250000, GCRuns: 8},
		Warnings:   []string{},
	}
}
