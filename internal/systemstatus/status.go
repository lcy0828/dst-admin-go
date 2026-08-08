package systemstatus

import (
	"time"

	"dont/internal/buildinfo"
)

type CPUStatus struct {
	Available   bool      `json:"available"`
	Error       string    `json:"error,omitempty"`
	Model       string    `json:"model"`
	Cores       int       `json:"cores"`
	Threads     int       `json:"threads"`
	Usage       float64   `json:"usage"`
	CoreUsage   []float64 `json:"coreUsage"`
	Load1       float64   `json:"load1"`
	Load5       float64   `json:"load5"`
	Load15      float64   `json:"load15"`
	LoadSupport bool      `json:"loadSupported"`
}

type MemoryStatus struct {
	Available      bool    `json:"available"`
	Error          string  `json:"error,omitempty"`
	TotalBytes     uint64  `json:"totalBytes"`
	UsedBytes      uint64  `json:"usedBytes"`
	AvailableBytes uint64  `json:"availableBytes"`
	Usage          float64 `json:"usage"`
}

type DiskStatus struct {
	Available      bool    `json:"available"`
	Error          string  `json:"error,omitempty"`
	Path           string  `json:"path"`
	TotalBytes     uint64  `json:"totalBytes"`
	UsedBytes      uint64  `json:"usedBytes"`
	AvailableBytes uint64  `json:"availableBytes"`
	Usage          float64 `json:"usage"`
}

type HostStatus struct {
	Available     bool   `json:"available"`
	Error         string `json:"error,omitempty"`
	Hostname      string `json:"hostname"`
	Platform      string `json:"platform"`
	Version       string `json:"version"`
	Kernel        string `json:"kernel"`
	Architecture  string `json:"architecture"`
	UptimeSeconds uint64 `json:"uptimeSeconds"`
}

type ProcessStatus struct {
	Available      bool    `json:"available"`
	Error          string  `json:"error,omitempty"`
	PID            int     `json:"pid"`
	UptimeSeconds  int64   `json:"uptimeSeconds"`
	CPUUsage       float64 `json:"cpuUsage"`
	MemoryRSSBytes uint64  `json:"memoryRssBytes"`
	MemoryVMSBytes uint64  `json:"memoryVmsBytes"`
	Threads        int32   `json:"threads"`
}

type RuntimeStatus struct {
	GoVersion       string `json:"goVersion"`
	Goroutines      int    `json:"goroutines"`
	HeapAllocBytes  uint64 `json:"heapAllocBytes"`
	SystemBytes     uint64 `json:"systemBytes"`
	HeapObjects     uint64 `json:"heapObjects"`
	LastGCPauseNano uint64 `json:"lastGcPauseNano"`
	GCRuns          uint32 `json:"gcRuns"`
}

type Status struct {
	ObservedAt  time.Time      `json:"observedAt"`
	Application buildinfo.Info `json:"application"`
	Host        HostStatus     `json:"host"`
	CPU         CPUStatus      `json:"cpu"`
	Memory      MemoryStatus   `json:"memory"`
	Disk        DiskStatus     `json:"disk"`
	Process     ProcessStatus  `json:"process"`
	Runtime     RuntimeStatus  `json:"runtime"`
	Warnings    []string       `json:"warnings"`
}

type Provider interface {
	Status() Status
}

type Service struct{ provider Provider }

func NewService(provider Provider) *Service { return &Service{provider: provider} }
func (s *Service) Status() Status {
	status := s.provider.Status()
	status.Application = buildinfo.Current()
	return status
}
