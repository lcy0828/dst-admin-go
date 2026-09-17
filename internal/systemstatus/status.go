package systemstatus

import (
	"errors"
	"sync"
	"time"

	"dont/internal/buildinfo"
)

type CPUStatus struct {
	Available      bool      `json:"available"`
	UsageAvailable bool      `json:"usageAvailable"`
	Error          string    `json:"error,omitempty"`
	Model          string    `json:"model"`
	Cores          int       `json:"cores"`
	Threads        int       `json:"threads"`
	Usage          float64   `json:"usage"`
	CoreUsage      []float64 `json:"coreUsage"`
	Load1          float64   `json:"load1"`
	Load5          float64   `json:"load5"`
	Load15         float64   `json:"load15"`
	LoadSupport    bool      `json:"loadSupported"`
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

type DatabaseStatus struct {
	Available               bool   `json:"available"`
	Error                   string `json:"error,omitempty"`
	Driver                  string `json:"driver"`
	JournalMode             string `json:"journalMode"`
	BusyTimeoutMilliseconds int    `json:"busyTimeoutMilliseconds"`
	ForeignKeys             bool   `json:"foreignKeys"`
	MaxOpenConnections      int    `json:"maxOpenConnections"`
	MigrationVersion        string `json:"migrationVersion"`
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
	Database    DatabaseStatus `json:"database"`
	Warnings    []string       `json:"warnings"`
}

type Provider interface {
	Status() Status
}

type DatabaseProvider func() (DatabaseStatus, error)

const statusCacheTTL = 1500 * time.Millisecond

type Service struct {
	provider Provider
	now      func() time.Time

	statusMu        sync.Mutex
	statusTTL       time.Duration
	statusValue     Status
	statusExpiresAt time.Time
	statusReady     bool
	statusInFlight  chan struct{}

	databaseMu        sync.Mutex
	database          DatabaseProvider
	databaseTTL       time.Duration
	databaseValue     DatabaseStatus
	databaseErr       error
	databaseExpiresAt time.Time
	databaseReady     bool
	databaseInFlight  chan struct{}
}

func NewService(provider Provider) *Service {
	return &Service{
		provider:    provider,
		now:         time.Now,
		statusTTL:   statusCacheTTL,
		databaseTTL: 30 * time.Second,
	}
}

func (s *Service) SetDatabaseProvider(provider DatabaseProvider) {
	s.databaseMu.Lock()
	s.database = provider
	s.databaseReady = false
	s.databaseMu.Unlock()

	s.statusMu.Lock()
	s.statusReady = false
	s.statusMu.Unlock()
}

func (s *Service) Status() Status {
	return s.readStatus(false)
}

func (s *Service) readStatus(refresh bool) Status {
	for {
		now := s.now()
		s.statusMu.Lock()
		if !refresh && s.statusReady && now.Before(s.statusExpiresAt) {
			status := cloneStatus(s.statusValue)
			s.statusMu.Unlock()
			return status
		}
		if inFlight := s.statusInFlight; inFlight != nil {
			s.statusMu.Unlock()
			<-inFlight
			refresh = false
			continue
		}
		inFlight := make(chan struct{})
		s.statusInFlight = inFlight
		s.statusMu.Unlock()

		status := s.collectStatus()
		s.statusMu.Lock()
		s.statusValue = cloneStatus(status)
		s.statusExpiresAt = s.now().Add(s.statusTTL)
		s.statusReady = true
		if s.statusInFlight == inFlight {
			s.statusInFlight = nil
			close(inFlight)
		}
		s.statusMu.Unlock()
		return status
	}
}

func (s *Service) collectStatus() Status {
	status := s.provider.Status()
	status.Application = buildinfo.Current()
	if database, err := s.databaseStatus(); err != nil {
		status.Database = DatabaseStatus{Error: err.Error()}
	} else {
		database.Available = true
		status.Database = database
	}
	return status
}

func (s *Service) databaseStatus() (DatabaseStatus, error) {
	for {
		now := s.now()
		s.databaseMu.Lock()
		if s.databaseReady && now.Before(s.databaseExpiresAt) {
			value, err := s.databaseValue, s.databaseErr
			s.databaseMu.Unlock()
			return value, err
		}
		if inFlight := s.databaseInFlight; inFlight != nil {
			s.databaseMu.Unlock()
			<-inFlight
			continue
		}
		provider := s.database
		if provider == nil {
			s.databaseMu.Unlock()
			return DatabaseStatus{}, errors.New("数据库状态提供器未配置")
		}
		inFlight := make(chan struct{})
		s.databaseInFlight = inFlight
		s.databaseMu.Unlock()

		value, err := provider()
		ttl := s.databaseTTL
		if err != nil {
			ttl = 2 * time.Second
		}
		s.databaseMu.Lock()
		s.databaseValue = value
		s.databaseErr = err
		s.databaseExpiresAt = s.now().Add(ttl)
		s.databaseReady = true
		if s.databaseInFlight == inFlight {
			s.databaseInFlight = nil
			close(inFlight)
		}
		s.databaseMu.Unlock()
		return value, err
	}
}

func cloneStatus(status Status) Status {
	status.CPU.CoreUsage = append([]float64(nil), status.CPU.CoreUsage...)
	status.Warnings = append([]string(nil), status.Warnings...)
	return status
}
