package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"dont/internal/jobs"
	"dont/internal/runtimeperformance"
	"dont/shared"

	"github.com/google/uuid"
)

const (
	keyRotationConfirmation = "ROTATE AGENT KEY"
	maximumOutputBytes      = 128 * 1024
)

var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var runtimeInstallationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var windowsAbsolutePathPattern = regexp.MustCompile(`(?i)^(?:[a-z]:[\\/]|\\\\)`)
var runtimePerformanceSHA256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Service struct {
	store              *Store
	jobs               *jobs.Service
	transport          Transport
	releases           *ReleaseStore
	now                func() time.Time
	localMu            sync.RWMutex
	localInstallations map[string]RuntimeConfig
	localDefaultID     string
	localProcesses     interface {
		ContainerProcesses(context.Context) ([]shared.ShardProcessReport, error)
	}
	localEnabled             bool
	runtimeAdoptionMu        sync.Mutex
	inventorySignalMu        sync.Mutex
	inventorySignals         map[string]string
	inventoryWake            chan struct{}
	runtimeTopologyMu        sync.RWMutex
	onRuntimeTopologyChanged []func()
	upgradeMu                sync.Mutex
	activeUpgrades           map[string]struct{}
	systemReportMu           sync.Mutex
	systemReports            map[string]*systemReportRequest
}

func (s *Service) ConfigureReleaseStore(store *ReleaseStore) error {
	if s == nil || store == nil {
		return errors.New("agent release store is required")
	}
	s.releases = store
	return nil
}

// ConfigureLocalRuntime sets the controller-local paths. It is intentionally
// separate from persisted Agent runtime configuration.
func (s *Service) ConfigureLocalRuntime(config RuntimeConfig) {
	config = normalizeRuntimeConfig(config)
	if !runtimeInstallationIDPattern.MatchString(config.InstallationID) {
		config.InstallationID = "default"
	}
	s.localMu.Lock()
	if s.localInstallations == nil {
		s.localInstallations = make(map[string]RuntimeConfig)
	}
	s.localInstallations[config.InstallationID] = config
	s.localDefaultID = config.InstallationID
	s.localEnabled = true
	s.localMu.Unlock()
	s.emitRuntimeTopologyChanged()
}

// ConfigureLocalRuntimeInstallation registers another DST installation on the
// controller machine without turning it into a separate machine target.
func (s *Service) ConfigureLocalRuntimeInstallation(config RuntimeConfig, makeDefault bool) error {
	config = normalizeRuntimeConfig(config)
	if !runtimeInstallationIDPattern.MatchString(config.InstallationID) {
		return ErrInvalidInput
	}
	s.localMu.Lock()
	if s.localInstallations == nil {
		s.localInstallations = make(map[string]RuntimeConfig)
	}
	s.localInstallations[config.InstallationID] = config
	if makeDefault || s.localDefaultID == "" {
		s.localDefaultID = config.InstallationID
	}
	s.localEnabled = true
	s.localMu.Unlock()
	s.emitRuntimeTopologyChanged()
	return nil
}

// ConfigureLocalContainerProcesses lets an embedded container Runtime report
// Shards that run outside the management container's process namespace.
func (s *Service) ConfigureLocalContainerProcesses(provider interface {
	ContainerProcesses(context.Context) ([]shared.ShardProcessReport, error)
}) {
	s.localProcesses = provider
}

// DisableLocalRuntime keeps control-plane APIs available without advertising
// a controller-local DST installation.
func (s *Service) DisableLocalRuntime() {
	s.localMu.Lock()
	s.localInstallations = make(map[string]RuntimeConfig)
	s.localDefaultID = ""
	s.localProcesses = nil
	s.localEnabled = false
	s.localMu.Unlock()
	s.emitRuntimeTopologyChanged()
}

// AddRuntimeTopologyListener subscribes control-plane projections to changes
// in execution targets or their cached inventories.
func (s *Service) AddRuntimeTopologyListener(callback func()) {
	if callback == nil {
		return
	}
	s.runtimeTopologyMu.Lock()
	s.onRuntimeTopologyChanged = append(s.onRuntimeTopologyChanged, callback)
	s.runtimeTopologyMu.Unlock()
}

func (s *Service) emitRuntimeTopologyChanged() {
	s.runtimeTopologyMu.RLock()
	listeners := append([]func(){}, s.onRuntimeTopologyChanged...)
	s.runtimeTopologyMu.RUnlock()
	for _, listener := range listeners {
		listener()
	}
}

func (s *Service) RuntimeTargets() ([]RuntimeTarget, error) {
	agentItems, _, err := s.Agents()
	if err != nil {
		return nil, err
	}
	configs, err := s.store.RuntimeConfigs()
	if err != nil {
		return nil, err
	}
	presentations, err := s.store.nodePresentations()
	if err != nil {
		return nil, err
	}
	items := make([]RuntimeTarget, 0, len(agentItems)+1)
	if s.localEnabled {
		items = append(items, applyRuntimeTargetPresentation(s.localRuntimeTarget(), presentations))
	}
	for _, agent := range agentItems {
		config, configured := configs[agent.ID]
		items = append(items, applyRuntimeTargetPresentation(runtimeTargetFromAgent(agent, config, configured), presentations))
	}
	return items, nil
}

func (s *Service) DefaultRuntimeTargetID(items []RuntimeTarget) string {
	if s.localEnabled {
		return "local"
	}
	for _, item := range items {
		if item.Configured && item.Online {
			return item.ID
		}
	}
	return ""
}

func (s *Service) RuntimeTarget(agentID string) (RuntimeTarget, error) {
	agent, err := s.Agent(agentID)
	if err != nil {
		return RuntimeTarget{}, err
	}
	config, err := s.store.RuntimeConfig(agentID)
	if errors.Is(err, ErrRuntimeNotConfigured) {
		return s.applyStoredRuntimeTargetPresentation(runtimeTargetFromAgent(agent, RuntimeConfig{}, false))
	}
	if err != nil {
		return RuntimeTarget{}, err
	}
	return s.applyStoredRuntimeTargetPresentation(runtimeTargetFromAgent(agent, config, true))
}

func (s *Service) SaveRuntimeConfig(agentID string, input RuntimeConfig) (RuntimeTarget, error) {
	return s.saveRuntimeConfig(agentID, input, RuntimeConfigSourceManual)
}

func (s *Service) saveRuntimeConfig(agentID string, input RuntimeConfig, source RuntimeConfigSource) (RuntimeTarget, error) {
	agent, err := s.Agent(agentID)
	if err != nil {
		return RuntimeTarget{}, err
	}
	config := normalizeRuntimeConfig(input)
	config.Source = source
	if config.DisplayName == "" {
		config.DisplayName = agent.Hostname
	}
	config, err = bindAdvertisedRuntimeInstallation(agent, config)
	if err != nil {
		return RuntimeTarget{}, err
	}
	if err := validateRuntimeConfig(config, agent.OS); err != nil {
		return RuntimeTarget{}, err
	}
	config, err = s.store.SaveRuntimeConfig(agentID, config)
	if err != nil {
		return RuntimeTarget{}, err
	}
	if err := s.store.DeleteInventory(agentID); err != nil {
		return RuntimeTarget{}, err
	}
	if source == RuntimeConfigSourceManual {
		if err := s.store.SetRuntimeAutoAdoptDisabled(agentID, false); err != nil {
			return RuntimeTarget{}, err
		}
	}
	s.emitRuntimeTopologyChanged()
	s.wakeInventoryRefresh()
	return s.applyStoredRuntimeTargetPresentation(runtimeTargetFromAgent(agent, config, true))
}

func (s *Service) RenameRuntimeTarget(targetID, displayName string) (RuntimeTarget, error) {
	targetID, displayName = strings.TrimSpace(targetID), strings.TrimSpace(displayName)
	if !validNodeDisplayName(displayName) {
		return RuntimeTarget{}, ErrInvalidInput
	}
	target, err := s.resolvePresentationTarget(targetID)
	if err != nil {
		return RuntimeTarget{}, err
	}
	if err := s.store.SaveNodeDisplayName(targetID, displayName); err != nil {
		return RuntimeTarget{}, err
	}
	target.Name, target.DisplayNameCustom = displayName, true
	return target, nil
}

func (s *Service) resolvePresentationTarget(targetID string) (RuntimeTarget, error) {
	var target RuntimeTarget
	switch {
	case targetID == "local":
		if !s.localEnabled {
			return RuntimeTarget{}, ErrRuntimeTargetNotFound
		}
		return s.applyStoredRuntimeTargetPresentation(s.localRuntimeTarget())
	case strings.HasPrefix(targetID, "agent:"):
		agentID := strings.TrimPrefix(targetID, "agent:")
		if !agentIDPattern.MatchString(agentID) {
			return RuntimeTarget{}, ErrRuntimeTargetNotFound
		}
		var err error
		target, err = s.RuntimeTarget(agentID)
		if errors.Is(err, ErrAgentNotFound) {
			return RuntimeTarget{}, ErrRuntimeTargetNotFound
		}
		if err != nil {
			return RuntimeTarget{}, err
		}
	default:
		return RuntimeTarget{}, ErrRuntimeTargetNotFound
	}

	return target, nil
}

func (s *Service) DeleteRuntimeConfig(agentID string) error {
	if _, err := s.Agent(agentID); err != nil {
		return err
	}
	if err := s.store.RemoveRuntimeConfiguration(agentID); err != nil {
		return err
	}
	s.emitRuntimeTopologyChanged()
	return nil
}

func (s *Service) localRuntimeTarget() RuntimeTarget {
	s.localMu.RLock()
	defaultID := s.localDefaultID
	config := s.localInstallations[defaultID]
	configs := make([]RuntimeConfig, 0, len(s.localInstallations))
	for _, installation := range s.localInstallations {
		configs = append(configs, installation)
	}
	s.localMu.RUnlock()
	sort.Slice(configs, func(i, j int) bool { return configs[i].InstallationID < configs[j].InstallationID })
	configured := config.SavePath != "" && config.ServerPath != ""
	ready := configured && existingDirectory(config.SavePath) && existingDirectory(config.ServerPath)
	status := RuntimeStatusConfigurationRequired
	if ready {
		status = RuntimeStatusReady
	}
	hostname, _ := os.Hostname()
	performance := runtimeperformance.Inspect(runtimeperformance.Options{
		ServerPath: config.ServerPath, ServerMode: config.ServerMode, WorkshopContentPath: config.WorkshopContentPath,
		Platform: runtime.GOOS, Architecture: runtime.GOARCH,
	})
	installations := make([]RuntimeInstallation, 0, len(configs))
	for _, installation := range configs {
		installationPerformance := runtimeperformance.Inspect(runtimeperformance.Options{
			ServerPath: installation.ServerPath, ServerMode: installation.ServerMode, WorkshopContentPath: installation.WorkshopContentPath,
			Platform: runtime.GOOS, Architecture: runtime.GOARCH,
		})
		installations = append(installations, RuntimeInstallation{
			ID: installation.InstallationID, Driver: "native", SavePath: installation.SavePath,
			ServerPath: installation.ServerPath, SteamCMDPath: installation.SteamCMDPath, UGCPath: installation.UGCPath,
			WorkshopContentPath: installation.WorkshopContentPath, ServerMode: installation.ServerMode,
			Performance: &installationPerformance,
		})
	}
	return RuntimeTarget{
		ID: "local", Kind: RuntimeKindLocal, Name: "本机", Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, Status: status,
		Default: true, DefaultInstallationID: defaultID, Configured: configured, Online: true,
		Containerized: localRuntimeContainerized(), IPAddresses: localRuntimeIPAddresses(), Capabilities: localRuntimeCapabilities(), Config: config, Installations: installations, Performance: &performance,
	}
}

func localRuntimeIPAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return []string{}
	}
	seen := make(map[string]bool)
	addresses := make([]string, 0, len(interfaces))
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagLoopback != 0 {
			continue
		}
		values, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, value := range values {
			ip, _, err := net.ParseCIDR(value.String())
			if err != nil {
				ip = net.ParseIP(value.String())
			}
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			text := ip.String()
			if !seen[text] {
				seen[text] = true
				addresses = append(addresses, text)
			}
		}
	}
	sort.Slice(addresses, func(i, j int) bool {
		left, right := net.ParseIP(addresses[i]), net.ParseIP(addresses[j])
		leftV4, rightV4 := left.To4() != nil, right.To4() != nil
		if leftV4 != rightV4 {
			return leftV4
		}
		return addresses[i] < addresses[j]
	})
	return addresses
}

func localRuntimeCapabilities() []string {
	capabilities := []string{
		"runtime.local", "system.report", "disk.inspect",
		"runtime.inventory.read", "runtime.processes.read", "runtime.capacity.read",
	}
	if runtime.GOOS == "windows" {
		return capabilities
	}
	return append(capabilities,
		"runtime.mods.local-link.v1",
		"shard.control.v1", "shard.control.v2", "shard.runtime-mode.v1", "shard.skip-mod-update.v1", "runtime.driver.v2", "runtime.console.v2",
		"runtime.logs.v1", "runtime.chat-history.v1", "runtime.artifacts.v1", "runtime.worldstate.read.v1", "runtime.entity-artwork.v1", "runtime.migration.v1", "runtime.migration.peer.v1",
		"runtime.backup.v1", "runtime.mods.v1", "runtime.mods.state.v1", "runtime.mods.files.v1", "runtime.mods.inventory.v1", "runtime.mods.fetch.v2", "runtime.mods.download.v1", "runtime.mods.content-publish.v1", "runtime.game-update.v1", "runtime.game-install.v1", "runtime.luajit.v2", "runtime.cpu.v1",
		"runtime.configuration.v1", "runtime.configuration.read.v1", "runtime.configuration.secrets.v1", "runtime.configuration.apply.v1",
		"runtime.maps.v1", "runtime.network.v1", "runtime.network.endpoints.v1", "runtime.migration.shard-routing.v1", "runtime.room-recovery.v1",
	)
}

func (s *Service) applyStoredRuntimeTargetPresentation(target RuntimeTarget) (RuntimeTarget, error) {
	presentations, err := s.store.nodePresentations()
	if err != nil {
		return RuntimeTarget{}, err
	}
	return applyRuntimeTargetPresentation(target, presentations), nil
}

func applyRuntimeTargetPresentation(target RuntimeTarget, presentations map[string]nodeDisplayNameRecord) RuntimeTarget {
	presentation := presentations[target.ID]
	if name := strings.TrimSpace(presentation.DisplayName); name != "" {
		target.Name, target.DisplayNameCustom = name, true
	}
	target.DisplayAddress = presentation.DisplayAddress
	return target
}

func effectiveAgentDisplayName(agent Agent, displayNames map[string]string) string {
	if displayName := strings.TrimSpace(displayNames["agent:"+agent.ID]); displayName != "" {
		return displayName
	}
	if hostname := strings.TrimSpace(agent.Hostname); hostname != "" {
		return hostname
	}
	return agent.ID
}

func validNodeDisplayName(value string) bool {
	if value == "" || utf8.RuneCountInString(value) > 100 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func NewService(store *Store, jobService *jobs.Service, transport Transport) (*Service, error) {
	if store == nil || jobService == nil || transport == nil {
		return nil, errors.New("agent dependencies are required")
	}
	if err := store.RecoverCommands(); err != nil {
		return nil, fmt.Errorf("recover agent commands: %w", err)
	}
	return &Service{
		store: store, jobs: jobService, transport: transport, now: time.Now,
		localInstallations: make(map[string]RuntimeConfig),
		inventorySignals:   make(map[string]string), inventoryWake: make(chan struct{}, 1),
		activeUpgrades: make(map[string]struct{}),
	}, nil
}

func (s *Service) Sync() (bool, error) {
	if !s.transport.Available() {
		changed, err := s.store.Sync(nil)
		if changed {
			s.emitRuntimeTopologyChanged()
		}
		if err == nil && s.updateInventorySignals(nil) {
			s.wakeInventoryRefresh()
		}
		return changed, err
	}
	snapshots, err := s.transport.Snapshots()
	if err != nil {
		changed, _ := s.store.Sync(nil)
		if changed {
			s.emitRuntimeTopologyChanged()
		}
		return changed, err
	}
	normalized := make([]TransportSnapshot, 0, len(snapshots))
	seen := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		if !agentIDPattern.MatchString(snapshot.ID) || seen[snapshot.ID] {
			continue
		}
		seen[snapshot.ID] = true
		snapshot.Hostname = trimLimit(snapshot.Hostname, 255)
		if snapshot.Hostname == "" {
			snapshot.Hostname = snapshot.ID
		}
		snapshot.OS = trimLimit(snapshot.OS, 64)
		snapshot.Arch = trimLimit(snapshot.Arch, 64)
		snapshot.Version = trimLimit(snapshot.Version, 128)
		if snapshot.Version == "" {
			snapshot.Version = "unknown"
		}
		if snapshot.Status != StatusOffline {
			snapshot.Status = StatusOnline
		}
		if snapshot.LastHeartbeat.IsZero() {
			snapshot.LastHeartbeat = s.now().UTC()
		}
		snapshot.IPAddresses = cleanStrings(snapshot.IPAddresses, 16, 128)
		snapshot.Capabilities = cleanStrings(snapshot.Capabilities, 64, 128)
		if snapshot.Details == nil {
			snapshot.Details = map[string]interface{}{}
		}
		normalized = append(normalized, snapshot)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	changed, err := s.store.Sync(normalized)
	if err != nil {
		return false, err
	}
	inventoryChanged := s.updateInventorySignals(normalized)
	adopted, err := s.adoptDiscoveredRuntimeConfigs()
	if err != nil {
		if changed {
			s.emitRuntimeTopologyChanged()
		}
		return changed, err
	}
	for _, value := range adopted {
		target := runtimeTargetFromAgent(value.agent, value.config, true)
		_, _ = s.submitInventoryRefresh(value.agent, runtimeConfigsForTarget(target))
	}
	changed = changed || len(adopted) > 0
	if changed {
		s.emitRuntimeTopologyChanged()
	}
	if inventoryChanged || len(adopted) > 0 {
		s.wakeInventoryRefresh()
	}
	return changed, nil
}

func (s *Service) updateInventorySignals(snapshots []TransportSnapshot) bool {
	next := make(map[string]string, len(snapshots))
	for _, snapshot := range snapshots {
		relevant := struct {
			Status        Status      `json:"status"`
			Version       string      `json:"version"`
			Capabilities  []string    `json:"capabilities"`
			Installations interface{} `json:"installations"`
		}{
			Status: snapshot.Status, Version: snapshot.Version, Capabilities: snapshot.Capabilities,
			Installations: snapshot.Details["runtime_installations"],
		}
		encoded, _ := json.Marshal(relevant)
		digest := sha256.Sum256(encoded)
		next[snapshot.ID] = hex.EncodeToString(digest[:])
	}
	s.inventorySignalMu.Lock()
	defer s.inventorySignalMu.Unlock()
	changed := len(next) != len(s.inventorySignals)
	if !changed {
		for agentID, signature := range next {
			if s.inventorySignals[agentID] != signature {
				changed = true
				break
			}
		}
	}
	s.inventorySignals = next
	return changed
}

func (s *Service) wakeInventoryRefresh() {
	select {
	case s.inventoryWake <- struct{}{}:
	default:
	}
}

type discoveredRuntimeAdoption struct {
	agent  Agent
	config RuntimeConfig
}

func (s *Service) adoptDiscoveredRuntimeConfigs() ([]discoveredRuntimeAdoption, error) {
	s.runtimeAdoptionMu.Lock()
	defer s.runtimeAdoptionMu.Unlock()

	configs, err := s.store.RuntimeConfigs()
	if err != nil {
		return nil, err
	}
	agentItems, err := s.store.Agents()
	if err != nil {
		return nil, err
	}
	adopted := make([]discoveredRuntimeAdoption, 0)
	for _, agent := range agentItems {
		if agent.Status != StatusOnline {
			continue
		}
		agent = s.decorateAgent(agent)
		if !agent.InstallationRegistrySupported {
			continue
		}

		previous, configured := configs[agent.ID]
		var installation RuntimeInstallation
		if configured {
			if previous.Source != RuntimeConfigSourceDiscovered {
				continue
			}
			var found bool
			installation, found = advertisedRuntimeInstallation(agent, previous.InstallationID)
			if !found {
				continue
			}
		} else {
			if agent.RuntimeAutoAdoptDisabled || len(agent.Installations) != 1 {
				continue
			}
			installation = agent.Installations[0]
		}

		config := discoveredRuntimeConfig(agent, installation, previous)
		if configured && config == normalizeRuntimeConfig(previous) {
			continue
		}
		config, err = bindAdvertisedRuntimeInstallation(agent, config)
		if err != nil || validateRuntimeConfig(config, agent.OS) != nil {
			continue
		}
		config, err = s.store.SaveRuntimeConfig(agent.ID, config)
		if err != nil {
			return adopted, err
		}
		if err := s.store.DeleteInventory(agent.ID); err != nil {
			return adopted, err
		}
		configs[agent.ID] = config
		adopted = append(adopted, discoveredRuntimeAdoption{agent: agent, config: config})
	}
	return adopted, nil
}

func advertisedRuntimeInstallation(agent Agent, installationID string) (RuntimeInstallation, bool) {
	for _, installation := range agent.Installations {
		if installation.ID == installationID {
			return installation, true
		}
	}
	return RuntimeInstallation{}, false
}

func discoveredRuntimeConfig(agent Agent, installation RuntimeInstallation, previous RuntimeConfig) RuntimeConfig {
	displayName := strings.TrimSpace(previous.DisplayName)
	if displayName == "" {
		displayName = nonEmpty(agent.Hostname, agent.ID)
	}
	return normalizeRuntimeConfig(RuntimeConfig{
		InstallationID:      installation.ID,
		DisplayName:         displayName,
		SavePath:            installation.SavePath,
		BackupPath:          previous.BackupPath,
		ServerPath:          installation.ServerPath,
		SteamCMDPath:        installation.SteamCMDPath,
		UGCPath:             installation.UGCPath,
		WorkshopContentPath: installation.WorkshopContentPath,
		ServerMode:          installation.ServerMode,
		LuaBinary:           nonEmpty(previous.LuaBinary, "lua"),
		LuaFallbackPath:     previous.LuaFallbackPath,
		Source:              RuntimeConfigSourceDiscovered,
	})
}

func (s *Service) Agents() ([]Agent, bool, error) {
	_, syncErr := s.Sync()
	items, err := s.store.Agents()
	if err != nil {
		return nil, s.transport.Available(), err
	}
	if syncErr != nil && len(items) == 0 {
		return nil, s.transport.Available(), syncErr
	}
	displayNames, err := s.store.NodeDisplayNames()
	if err != nil {
		return nil, s.transport.Available(), err
	}
	latestByPlatform := s.latestAgentReleasesByPlatform()
	for index := range items {
		items[index].DisplayName = effectiveAgentDisplayName(items[index], displayNames)
		items[index] = s.decorateAgentWithRelease(items[index], latestByPlatform[agentReleasePlatformKey(items[index].OS, items[index].Arch)])
	}
	return items, s.transport.Available(), nil
}

func (s *Service) Agent(id string) (Agent, error) {
	if !agentIDPattern.MatchString(id) {
		return Agent{}, ErrAgentNotFound
	}
	_, _ = s.Sync()
	item, err := s.store.Agent(id)
	if err != nil {
		return Agent{}, err
	}
	displayNames, err := s.store.NodeDisplayNames()
	if err != nil {
		return Agent{}, err
	}
	item.DisplayName = effectiveAgentDisplayName(item, displayNames)
	return s.decorateAgent(item), nil
}

func (s *Service) Forget(id string) error {
	if !agentIDPattern.MatchString(id) {
		return ErrAgentNotFound
	}
	_, _ = s.Sync()
	if err := s.store.DeleteAgent(id); err != nil {
		return err
	}
	if forgetter, ok := s.transport.(interface{ ForgetSnapshot(string) }); ok {
		forgetter.ForgetSnapshot(id)
	}
	s.emitRuntimeTopologyChanged()
	return nil
}

func (s *Service) Actions() []ActionDefinition {
	return []ActionDefinition{
		{ID: ActionSystemRefresh, Name: "刷新系统信息", Description: "请求节点重新上报主机、运行时间和内存信息", Platforms: []string{"linux", "darwin", "windows"}},
		{ID: ActionDiskInspect, Name: "检查磁盘", Description: "以参数数组执行只读磁盘容量检查", Platforms: []string{"linux", "darwin", "windows"}},
	}
}

func (s *Service) RunCommand(agentID string, input CommandInput) (jobs.Job, error) {
	agent, err := s.Agent(agentID)
	if err != nil {
		return jobs.Job{}, err
	}
	if agent.Status != StatusOnline {
		return jobs.Job{}, ErrAgentOffline
	}
	if !supportsAction(input.Action, agent.OS) {
		return jobs.Job{}, ErrUnsupportedAction
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 30
	}
	if input.TimeoutSeconds < 5 || input.TimeoutSeconds > 300 {
		return jobs.Job{}, ErrInvalidInput
	}
	now := s.now().UTC()
	command := Command{ID: uuid.NewString(), AgentID: agent.ID, AgentName: agent.DisplayName, Action: input.Action, Status: CommandQueued, CreatedAt: now}
	if err := s.store.CreateCommand(command); err != nil {
		return jobs.Job{}, err
	}
	job, err := s.jobs.SubmitFactory("agent.command", "", "", []jobs.TargetSpec{{ID: agent.ID, Name: agent.DisplayName}}, func(job jobs.Job) jobs.Runner {
		_ = s.store.AttachJob(command.ID, job.ID)
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			started := s.now().UTC()
			_ = s.store.StartCommand(command.ID, started)
			taskContext, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
			defer cancel()
			result, executeErr := s.transport.Execute(taskContext, agent.ID, input.Action, input.TimeoutSeconds)
			result.Output = truncateBytes(strings.TrimSpace(result.Output), maximumOutputBytes)
			if result.RemoteID != "" {
				_ = s.store.SetRemoteID(command.ID, result.RemoteID)
			}
			finished := s.now().UTC()
			if executeErr != nil {
				status := CommandFailed
				jobStatus := jobs.StatusFailed
				code := "AGENT_COMMAND_FAILED"
				message := executeErr.Error()
				if errors.Is(taskContext.Err(), context.Canceled) {
					status, jobStatus, code, message = CommandCanceled, jobs.StatusCanceled, "AGENT_COMMAND_CANCELED", "Agent 命令已取消"
					result.ExitCode = 130
				} else if errors.Is(taskContext.Err(), context.DeadlineExceeded) {
					code, message = "AGENT_COMMAND_TIMEOUT", "Agent 命令执行超时"
					result.ExitCode = 124
				} else if result.ExitCode == 0 {
					result.ExitCode = 1
				}
				_ = s.store.FinishCommand(command.ID, status, result, message, finished)
				report(jobs.TargetResult{TargetID: agent.ID, Status: jobStatus, Error: &jobs.Error{Code: code, Message: message}})
				return executeErr
			}
			_ = s.store.FinishCommand(command.ID, CommandSucceeded, result, "", finished)
			report(jobs.TargetResult{TargetID: agent.ID, Status: jobs.StatusSucceeded, Message: commandSuccessMessage(input.Action)})
			_, _ = s.Sync()
			return nil
		}
	})
	if err != nil {
		_ = s.store.FinishCommand(command.ID, CommandFailed, ExecutionResult{ExitCode: 1}, err.Error(), s.now().UTC())
		return jobs.Job{}, err
	}
	return job, nil
}

func (s *Service) Commands(filter CommandFilter) (CommandList, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 ||
		(filter.AgentID != "" && !agentIDPattern.MatchString(filter.AgentID)) ||
		(filter.Status != "" && !validCommandStatus(filter.Status)) ||
		utf8.RuneCountInString(filter.Query) > 100 ||
		(filter.StartAt != nil && filter.EndAt != nil && !filter.StartAt.Before(*filter.EndAt)) {
		return CommandList{}, ErrInvalidInput
	}
	return s.store.Commands(filter)
}

func (s *Service) Command(id string) (Command, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return Command{}, ErrCommandNotFound
	}
	return s.store.Command(id)
}

func (s *Service) Security() (SecurityStatus, error) {
	items, _, err := s.Agents()
	if err != nil && !errors.Is(err, ErrUnavailable) {
		return SecurityStatus{}, err
	}
	online := 0
	for _, item := range items {
		if item.Status == StatusOnline {
			online++
		}
	}
	status := SecurityStatus{Available: s.transport.Available(), ConnectedAgents: online}
	if !s.transport.Available() {
		return status, nil
	}
	key, err := s.transport.CurrentKey()
	if err != nil {
		return SecurityStatus{}, err
	}
	if key == "" {
		return status, nil
	}
	status.Configured = true
	status.MaskedKey = maskKey(key)
	status.Fingerprint = fingerprint(key)
	record, err := s.store.Security()
	if err != nil {
		return SecurityStatus{}, err
	}
	if record.Fingerprint == status.Fingerprint && !record.RotatedAt.IsZero() {
		status.RotatedAt = utcTimePointer(record.RotatedAt)
	}
	return status, nil
}

func (s *Service) RotateKey(ctx context.Context, input RotateKeyInput) (RotateKeyResult, error) {
	if strings.TrimSpace(input.Confirmation) != keyRotationConfirmation {
		return RotateKeyResult{}, ErrConfirmationRequired
	}
	if !s.transport.Available() {
		return RotateKeyResult{}, ErrUnavailable
	}
	key, err := s.transport.RotateKey(ctx)
	if err != nil {
		return RotateKeyResult{}, err
	}
	if len(key) < 32 {
		return RotateKeyResult{}, errors.New("agent transport returned an invalid key")
	}
	now := s.now().UTC()
	digest := fingerprint(key)
	if err := s.store.SaveSecurity(digest, now); err != nil {
		return RotateKeyResult{}, err
	}
	return RotateKeyResult{NewKey: key, Fingerprint: digest, RotatedAt: now}, nil
}

func (s *Service) StartWatcher(ctx context.Context, interval time.Duration, notify func()) {
	go s.Watch(ctx, interval, notify)
}

func (s *Service) Watch(ctx context.Context, interval time.Duration, notify func()) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, _ := s.Sync()
			if changed && notify != nil {
				notify()
			}
		}
	}
}

func supportsAction(action Action, platform string) bool {
	switch action {
	case ActionSystemRefresh:
		return true
	case ActionDiskInspect:
		platform = strings.ToLower(strings.TrimSpace(platform))
		return platform == "linux" || platform == "darwin" || platform == "windows"
	default:
		return false
	}
}

func validCommandStatus(status CommandStatus) bool {
	return status == CommandQueued || status == CommandRunning || status == CommandSucceeded || status == CommandFailed || status == CommandCanceled
}

func commandSuccessMessage(action Action) string {
	if action == ActionDiskInspect {
		return "磁盘检查已完成"
	}
	return "系统信息已刷新"
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "********"
	}
	return key[:4] + strings.Repeat("*", 12) + key[len(key)-4:]
}

func fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func trimLimit(value string, maximum int) string {
	value = strings.TrimSpace(value)
	for utf8.RuneCountInString(value) > maximum {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

func cleanStrings(values []string, maximumItems, maximumLength int) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		value = trimLimit(value, maximumLength)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
		if len(result) == maximumItems {
			break
		}
	}
	sort.Strings(result)
	return result
}

func truncateBytes(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "\n[输出已截断]"
}

func normalizeRuntimeConfig(config RuntimeConfig) RuntimeConfig {
	config.InstallationID = strings.TrimSpace(config.InstallationID)
	if config.InstallationID == "" {
		config.InstallationID = "default"
	}
	config.DisplayName = strings.TrimSpace(config.DisplayName)
	config.SavePath = strings.TrimSpace(config.SavePath)
	config.BackupPath = strings.TrimSpace(config.BackupPath)
	config.ServerPath = strings.TrimSpace(config.ServerPath)
	config.UGCPath = strings.TrimSpace(config.UGCPath)
	config.SteamCMDPath = strings.TrimSpace(config.SteamCMDPath)
	config.WorkshopContentPath = strings.TrimSpace(config.WorkshopContentPath)
	config.LuaBinary = strings.TrimSpace(config.LuaBinary)
	config.LuaFallbackPath = strings.TrimSpace(config.LuaFallbackPath)
	config.ServerMode = strings.ToLower(strings.TrimSpace(config.ServerMode))
	if config.Source != RuntimeConfigSourceDiscovered {
		config.Source = RuntimeConfigSourceManual
	}
	if config.LuaBinary == "" {
		config.LuaBinary = "lua"
	}
	if config.ServerMode == "" {
		config.ServerMode = "64"
	}
	config.UpdatedAt = nil
	return config
}

func validateRuntimeConfig(config RuntimeConfig, platform string) error {
	if !runtimeInstallationIDPattern.MatchString(config.InstallationID) ||
		config.DisplayName == "" || utf8.RuneCountInString(config.DisplayName) > 100 ||
		config.SavePath == "" || config.ServerPath == "" ||
		utf8.RuneCountInString(config.LuaBinary) > 255 ||
		(config.ServerMode != "32" && config.ServerMode != "64") ||
		(config.Source != RuntimeConfigSourceManual && config.Source != RuntimeConfigSourceDiscovered) {
		return ErrInvalidInput
	}
	if strings.ContainsAny(config.DisplayName+config.LuaBinary, "\x00\r\n") {
		return ErrInvalidInput
	}
	for _, path := range []string{
		config.SavePath, config.BackupPath, config.ServerPath, config.UGCPath,
		config.SteamCMDPath, config.WorkshopContentPath, config.LuaFallbackPath,
	} {
		if path == "" {
			continue
		}
		if utf8.RuneCountInString(path) > 2048 || strings.ContainsAny(path, "\x00\r\n") || !absoluteRuntimePath(path, platform) {
			return ErrInvalidInput
		}
	}
	return nil
}

func absoluteRuntimePath(value, platform string) bool {
	if strings.EqualFold(strings.TrimSpace(platform), "windows") {
		return windowsAbsolutePathPattern.MatchString(value)
	}
	return strings.HasPrefix(value, "/")
}

func runtimeTargetFromAgent(agent Agent, config RuntimeConfig, configured bool) RuntimeTarget {
	status := RuntimeStatusConfigurationRequired
	if configured {
		status = RuntimeStatusReady
		if agent.Status != StatusOnline {
			status = RuntimeStatusOffline
		} else if _, err := bindAdvertisedRuntimeInstallation(agent, config); err != nil {
			status = RuntimeStatusConfigurationRequired
		}
	}
	name := strings.TrimSpace(agent.DisplayName)
	if name == "" {
		name = strings.TrimSpace(agent.Hostname)
	}
	if name == "" {
		name = agent.ID
	}
	var performance *shared.RuntimePerformanceReport
	if configured {
		for _, installation := range agent.Installations {
			if installation.ID == config.InstallationID {
				performance = cloneRuntimePerformance(installation.Performance)
				break
			}
		}
	}
	heartbeat := agent.LastHeartbeat.UTC()
	return RuntimeTarget{
		ID: "agent:" + agent.ID, Kind: RuntimeKindAgent, AgentID: agent.ID, Name: name,
		Containerized: agent.Details["deployment_profile"] == "container", Hostname: agent.Hostname, OS: agent.OS, Arch: agent.Arch, IPAddresses: append([]string{}, agent.IPAddresses...), Status: status,
		DefaultInstallationID: config.InstallationID, Configured: configured, Online: agent.Status == StatusOnline,
		Capabilities: append([]string(nil), agent.Capabilities...), LastHeartbeat: &heartbeat, Config: config,
		Installations: cloneRuntimeInstallations(agent.Installations), Performance: performance,
	}
}

func cloneRuntimeInstallations(values []RuntimeInstallation) []RuntimeInstallation {
	result := make([]RuntimeInstallation, len(values))
	for index, value := range values {
		result[index] = value
		result[index].Performance = cloneRuntimePerformance(value.Performance)
	}
	return result
}

func cloneRuntimePerformance(value *shared.RuntimePerformanceReport) *shared.RuntimePerformanceReport {
	if value == nil {
		return nil
	}
	clone := *value
	clone.SupportedModes = append([]shared.RuntimePerformanceMode(nil), value.SupportedModes...)
	clone.Issues = append([]string(nil), value.Issues...)
	return &clone
}

func (s *Service) runtimeConfigForAgent(agent Agent) (RuntimeConfig, error) {
	config, err := s.store.RuntimeConfig(agent.ID)
	if err != nil {
		return RuntimeConfig{}, err
	}
	return bindAdvertisedRuntimeInstallation(agent, config)
}

func advertisedRuntimeInstallations(details map[string]interface{}) []RuntimeInstallation {
	if details == nil || details["runtime_installations"] == nil {
		return []RuntimeInstallation{}
	}
	type report struct {
		ID                  string                           `json:"id"`
		Driver              string                           `json:"driver"`
		SavePath            string                           `json:"save_path"`
		ServerPath          string                           `json:"server_path"`
		SteamCMDPath        string                           `json:"steamcmd_path"`
		UGCPath             string                           `json:"ugc_path"`
		WorkshopContentPath string                           `json:"workshop_content_path"`
		ServerMode          string                           `json:"server_mode"`
		Performance         *shared.RuntimePerformanceReport `json:"performance"`
	}
	encoded, err := json.Marshal(details["runtime_installations"])
	if err != nil {
		return []RuntimeInstallation{}
	}
	var reports []report
	if err := json.Unmarshal(encoded, &reports); err != nil {
		return []RuntimeInstallation{}
	}
	result := make([]RuntimeInstallation, 0, len(reports))
	seen := make(map[string]bool, len(reports))
	for _, value := range reports {
		value.ID = strings.TrimSpace(value.ID)
		value.Driver = strings.ToLower(strings.TrimSpace(value.Driver))
		value.ServerMode = strings.TrimSpace(value.ServerMode)
		if !runtimeInstallationIDPattern.MatchString(value.ID) || seen[value.ID] || (value.Driver != "native" && value.Driver != "container") ||
			(value.ServerMode != "32" && value.ServerMode != "64") {
			continue
		}
		paths := []*string{&value.SavePath, &value.ServerPath, &value.SteamCMDPath, &value.UGCPath, &value.WorkshopContentPath}
		valid := true
		for _, path := range paths {
			*path = strings.TrimSpace(*path)
			if *path != "" && (utf8.RuneCountInString(*path) > 2048 || strings.ContainsAny(*path, "\x00\r\n")) {
				valid = false
			}
		}
		if !valid || value.SavePath == "" || value.ServerPath == "" {
			continue
		}
		seen[value.ID] = true
		result = append(result, RuntimeInstallation{
			ID: value.ID, Driver: value.Driver, SavePath: value.SavePath, ServerPath: value.ServerPath,
			SteamCMDPath: value.SteamCMDPath, UGCPath: value.UGCPath,
			WorkshopContentPath: value.WorkshopContentPath, ServerMode: value.ServerMode, Performance: normalizeRuntimePerformance(value.Performance),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func normalizeRuntimePerformance(value *shared.RuntimePerformanceReport) *shared.RuntimePerformanceReport {
	if value == nil || (value.Provider != "game" && value.Provider != "dontstarve-luajit2") || !validRuntimePerformanceStatus(value.Status) {
		return nil
	}
	clone := *value
	for _, text := range []*string{&clone.PackageVersion, &clone.GameVersion, &clone.SignatureVersion} {
		*text = strings.TrimSpace(*text)
		if utf8.RuneCountInString(*text) > 64 || strings.ContainsAny(*text, "\x00\r\n") {
			return nil
		}
	}
	clone.BinarySHA256 = strings.ToLower(strings.TrimSpace(clone.BinarySHA256))
	if clone.BinarySHA256 != "" && !runtimePerformanceSHA256Pattern.MatchString(clone.BinarySHA256) {
		return nil
	}
	var modesValid bool
	clone.SupportedModes, modesValid = cleanRuntimePerformanceModes(clone.SupportedModes)
	if !modesValid {
		return nil
	}
	var issuesValid bool
	clone.Issues, issuesValid = cleanRuntimePerformanceIssues(clone.Issues)
	if !issuesValid {
		return nil
	}
	if clone.Status != shared.RuntimePerformanceReady {
		clone.CanEnable = false
		return &clone
	}
	signatureReady := clone.SignatureVersion != "" && clone.GameVersion == clone.SignatureVersion
	if clone.AutomaticSignatures {
		signatureReady = true
	}
	if !clone.CanEnable || clone.Provider != "dontstarve-luajit2" || clone.PackageVersion == "" ||
		clone.GameVersion == "" || !signatureReady ||
		clone.BinarySHA256 == "" || len(clone.Issues) != 0 || !hasRequiredRuntimePerformanceModes(clone.SupportedModes) {
		return nil
	}
	return &clone
}

func validRuntimePerformanceStatus(value shared.RuntimePerformanceStatus) bool {
	return value == shared.RuntimePerformanceNotInstalled || value == shared.RuntimePerformanceDetectedUnverified ||
		value == shared.RuntimePerformanceIncompatible || value == shared.RuntimePerformanceReady
}

func cleanRuntimePerformanceModes(values []shared.RuntimePerformanceMode) ([]shared.RuntimePerformanceMode, bool) {
	seen := make(map[shared.RuntimePerformanceMode]bool, len(values))
	result := make([]shared.RuntimePerformanceMode, 0, len(values))
	for _, value := range values {
		if value != shared.RuntimePerformanceModeGame && value != shared.RuntimePerformanceModeJITOff && value != shared.RuntimePerformanceModeJITOn && value != shared.RuntimePerformanceModeArenaGC {
			return nil, false
		}
		if !seen[value] && len(result) < 4 {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result, true
}

func hasRequiredRuntimePerformanceModes(values []shared.RuntimePerformanceMode) bool {
	required := map[shared.RuntimePerformanceMode]bool{
		shared.RuntimePerformanceModeGame:   false,
		shared.RuntimePerformanceModeJITOff: false,
		shared.RuntimePerformanceModeJITOn:  false,
	}
	for _, value := range values {
		if _, ok := required[value]; ok {
			required[value] = true
		}
	}
	for _, present := range required {
		if !present {
			return false
		}
	}
	return true
}

func cleanRuntimePerformanceIssues(values []string) ([]string, bool) {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !validRuntimePerformanceIssue(value) {
			return nil, false
		}
		if seen[value] {
			continue
		}
		if len(result) >= 16 {
			return nil, false
		}
		seen[value] = true
		result = append(result, value)
	}
	return result, true
}

func validRuntimePerformanceIssue(value string) bool {
	switch value {
	case "server_architecture_unsupported", "architecture_unsupported", "platform_not_verified", "installation_incomplete",
		"injector_wrapper_invalid", "signature_unreadable", "game_version_unknown", "signature_version_mismatch",
		"package_version_unknown", "binary_hash_unavailable", "plugin_layout_unverified", "injector_marker_invalid", "runtime_mode_contract_missing":
		return true
	default:
		return false
	}
}

func runtimeInstallationRegistrySupported(details map[string]interface{}) bool {
	if details == nil {
		return false
	}
	_, exists := details["runtime_installations"]
	return exists
}

func bindAdvertisedRuntimeInstallation(agent Agent, config RuntimeConfig) (RuntimeConfig, error) {
	if !agent.InstallationRegistrySupported {
		return config, nil
	}
	var selected *RuntimeInstallation
	for index := range agent.Installations {
		if agent.Installations[index].ID == config.InstallationID {
			selected = &agent.Installations[index]
			break
		}
	}
	if selected == nil {
		return RuntimeConfig{}, ErrRuntimeInstallationNotRegistered
	}
	bindings := []struct {
		configured *string
		advertised string
	}{
		{&config.SavePath, selected.SavePath}, {&config.ServerPath, selected.ServerPath},
		{&config.SteamCMDPath, selected.SteamCMDPath}, {&config.UGCPath, selected.UGCPath},
		{&config.WorkshopContentPath, selected.WorkshopContentPath}, {&config.ServerMode, selected.ServerMode},
	}
	for _, binding := range bindings {
		if *binding.configured != "" && *binding.configured != binding.advertised {
			return RuntimeConfig{}, ErrRuntimeInstallationNotRegistered
		}
		if *binding.configured == "" {
			*binding.configured = binding.advertised
		}
	}
	return config, nil
}

func existingDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func utcTimePointer(value time.Time) *time.Time { utc := value.UTC(); return &utc }
