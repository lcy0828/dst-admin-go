package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"dont/internal/jobs"
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

type Service struct {
	store          *Store
	jobs           *jobs.Service
	transport      Transport
	now            func() time.Time
	local          RuntimeConfig
	localProcesses interface {
		ContainerProcesses(context.Context) ([]shared.ShardProcessReport, error)
	}
	localEnabled bool
}

// ConfigureLocalRuntime sets the controller-local paths. It is intentionally
// separate from persisted Agent runtime configuration.
func (s *Service) ConfigureLocalRuntime(config RuntimeConfig) {
	s.local = normalizeRuntimeConfig(config)
	s.localEnabled = true
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
	s.local = RuntimeConfig{}
	s.localProcesses = nil
	s.localEnabled = false
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
	displayNames, err := s.store.NodeDisplayNames()
	if err != nil {
		return nil, err
	}
	items := make([]RuntimeTarget, 0, len(agentItems)+1)
	if s.localEnabled {
		items = append(items, applyRuntimeTargetDisplayName(s.localRuntimeTarget(), displayNames))
	}
	for _, agent := range agentItems {
		config, configured := configs[agent.ID]
		items = append(items, applyRuntimeTargetDisplayName(runtimeTargetFromAgent(agent, config, configured), displayNames))
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
		return s.applyStoredRuntimeTargetDisplayName(runtimeTargetFromAgent(agent, RuntimeConfig{}, false))
	}
	if err != nil {
		return RuntimeTarget{}, err
	}
	return s.applyStoredRuntimeTargetDisplayName(runtimeTargetFromAgent(agent, config, true))
}

func (s *Service) SaveRuntimeConfig(agentID string, input RuntimeConfig) (RuntimeTarget, error) {
	agent, err := s.Agent(agentID)
	if err != nil {
		return RuntimeTarget{}, err
	}
	config := normalizeRuntimeConfig(input)
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
	return s.applyStoredRuntimeTargetDisplayName(runtimeTargetFromAgent(agent, config, true))
}

func (s *Service) RenameRuntimeTarget(targetID, displayName string) (RuntimeTarget, error) {
	targetID = strings.TrimSpace(targetID)
	displayName = strings.TrimSpace(displayName)
	if !validNodeDisplayName(displayName) {
		return RuntimeTarget{}, ErrInvalidInput
	}

	var target RuntimeTarget
	switch {
	case targetID == "local":
		if !s.localEnabled {
			return RuntimeTarget{}, ErrRuntimeTargetNotFound
		}
		target = s.localRuntimeTarget()
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

	if err := s.store.SaveNodeDisplayName(targetID, displayName); err != nil {
		return RuntimeTarget{}, err
	}
	target.Name = displayName
	return target, nil
}

func (s *Service) DeleteRuntimeConfig(agentID string) error {
	if _, err := s.Agent(agentID); err != nil {
		return err
	}
	if err := s.store.DeleteRuntimeConfig(agentID); err != nil {
		return err
	}
	return s.store.DeleteInventory(agentID)
}

func (s *Service) localRuntimeTarget() RuntimeTarget {
	configured := s.local.SavePath != "" && s.local.ServerPath != ""
	ready := configured && existingDirectory(s.local.SavePath) && existingDirectory(s.local.ServerPath)
	status := RuntimeStatusConfigurationRequired
	if ready {
		status = RuntimeStatusReady
	}
	hostname, _ := os.Hostname()
	return RuntimeTarget{
		ID: "local", Kind: RuntimeKindLocal, Name: "本机", Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, Status: status,
		Default: true, Configured: configured, Online: true,
		Capabilities: []string{"runtime.local"}, Config: s.local,
	}
}

func (s *Service) applyStoredRuntimeTargetDisplayName(target RuntimeTarget) (RuntimeTarget, error) {
	displayNames, err := s.store.NodeDisplayNames()
	if err != nil {
		return RuntimeTarget{}, err
	}
	return applyRuntimeTargetDisplayName(target, displayNames), nil
}

func applyRuntimeTargetDisplayName(target RuntimeTarget, displayNames map[string]string) RuntimeTarget {
	if displayName := strings.TrimSpace(displayNames[target.ID]); displayName != "" {
		target.Name = displayName
	}
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
	return &Service{store: store, jobs: jobService, transport: transport, now: time.Now}, nil
}

func (s *Service) Sync() (bool, error) {
	if !s.transport.Available() {
		return s.store.Sync(nil)
	}
	snapshots, err := s.transport.Snapshots()
	if err != nil {
		_, _ = s.store.Sync(nil)
		return false, err
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
	return s.store.Sync(normalized)
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
	for index := range items {
		items[index].DisplayName = effectiveAgentDisplayName(items[index], displayNames)
		items[index] = s.decorateAgent(items[index])
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
		(config.ServerMode != "32" && config.ServerMode != "64" && config.ServerMode != "luajit") {
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
	heartbeat := agent.LastHeartbeat.UTC()
	return RuntimeTarget{
		ID: "agent:" + agent.ID, Kind: RuntimeKindAgent, AgentID: agent.ID, Name: name,
		Hostname: agent.Hostname, OS: agent.OS, Arch: agent.Arch, Status: status,
		Configured: configured, Online: agent.Status == StatusOnline,
		Capabilities: append([]string(nil), agent.Capabilities...), LastHeartbeat: &heartbeat, Config: config,
	}
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
		ID                  string `json:"id"`
		Driver              string `json:"driver"`
		SavePath            string `json:"save_path"`
		ServerPath          string `json:"server_path"`
		SteamCMDPath        string `json:"steamcmd_path"`
		UGCPath             string `json:"ugc_path"`
		WorkshopContentPath string `json:"workshop_content_path"`
		ServerMode          string `json:"server_mode"`
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
			WorkshopContentPath: value.WorkshopContentPath, ServerMode: value.ServerMode,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
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
