package systemsettings

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/deploymentprofile"
)

var digitsPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var secretPattern = regexp.MustCompile(`^[A-Za-z0-9_+=./:-]{8,512}$`)
var colorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}([0-9A-Fa-f]{2})?$`)
var rgbaPattern = regexp.MustCompile(`^rgba\(\s*(25[0-5]|2[0-4][0-9]|1?[0-9]{1,2})\s*,\s*(25[0-5]|2[0-4][0-9]|1?[0-9]{1,2})\s*,\s*(25[0-5]|2[0-4][0-9]|1?[0-9]{1,2})\s*,\s*(0|1|0?\.[0-9]+)\s*\)$`)

type Service struct {
	applyMu       sync.Mutex
	baselineMu    sync.RWMutex
	applier       RuntimeApplier
	repository    Repository
	startupValues map[string]string
	lookupEnv     func(string) (string, bool)
	now           func() time.Time
}

// RuntimeApplier runs persistence and runtime replacement within one admission
// barrier. On failure it must restore the old runtime and call rollback if saved.
type RuntimeApplier func(context.Context, func() error, func() error) error

func (s *Service) SetRuntimeApplier(applier RuntimeApplier) { s.applier = applier }

func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("system settings repository is required")
	}
	snapshot, err := repository.Snapshot()
	if err != nil {
		return nil, err
	}
	startupValues := make(map[string]string, len(snapshot.Values))
	for id, value := range snapshot.Values {
		startupValues[id] = value
	}
	return &Service{repository: repository, startupValues: startupValues, lookupEnv: os.LookupEnv, now: time.Now}, nil
}

func (s *Service) Settings() (Settings, error) {
	snapshot, err := s.repository.Snapshot()
	if err != nil {
		return Settings{}, err
	}
	return s.settingsFromSnapshot(snapshot), nil
}

func (s *Service) Preview(input Input) (Preview, error) {
	snapshot, err := s.repository.Snapshot()
	if err != nil {
		return Preview{}, err
	}
	return s.preview(snapshot, input)
}

func (s *Service) Apply(input Input) (ApplyResult, error) {
	return s.ApplyContext(context.Background(), input)
}

func (s *Service) ApplyContext(ctx context.Context, input Input) (ApplyResult, error) {
	if !s.applyMu.TryLock() {
		return ApplyResult{}, ErrRuntimeBusy
	}
	defer s.applyMu.Unlock()
	if strings.TrimSpace(input.Confirmation) != ApplyConfirmation {
		return ApplyResult{}, ErrConfirmationRequired
	}
	snapshot, err := s.repository.Snapshot()
	if err != nil {
		return ApplyResult{}, err
	}
	preview, err := s.preview(snapshot, input)
	if err != nil {
		return ApplyResult{}, err
	}
	if !preview.Valid {
		return ApplyResult{}, ErrInvalidInput
	}
	updates, err := s.normalizedUpdates(snapshot, input)
	if err != nil {
		return ApplyResult{}, err
	}
	previous := snapshot
	persist := func() error {
		if len(updates) == 0 {
			return nil
		}
		snapshot, err = s.repository.Save(previous.Revision, updates)
		return err
	}
	rollback := func() error {
		values := make(map[string]string, len(updates))
		for id := range updates {
			values[id] = previous.Values[id]
		}
		_, rollbackErr := s.repository.Save(snapshot.Revision, values)
		return rollbackErr
	}
	combined := make(map[string]string, len(snapshot.Values))
	for id, value := range snapshot.Values {
		combined[id] = value
	}
	for id, value := range updates {
		combined[id] = value
	}
	if s.applier != nil && s.runtimeChanged(combined) {
		if err = s.applier(ctx, persist, rollback); err == nil {
			s.baselineMu.Lock()
			s.startupValues = combined
			s.baselineMu.Unlock()
		}
	} else {
		err = persist()
	}
	if err != nil {
		return ApplyResult{}, err
	}
	// Runtime initialization may persist generated node identity or gateway
	// credentials in this file. Return the current revision for the next edit.
	snapshot, err = s.repository.Snapshot()
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Settings: s.settingsFromSnapshot(snapshot), Changes: preview.Changes}, nil
}

func (s *Service) runtimeChanged(values map[string]string) bool {
	s.baselineMu.RLock()
	defer s.baselineMu.RUnlock()
	for _, definition := range fieldDefinitions {
		if definition.RestartRequired && s.effectiveValue(definition.ID, values[definition.ID]) != s.effectiveValue(definition.ID, s.startupValues[definition.ID]) {
			return true
		}
	}
	return false
}

func (s *Service) preview(snapshot Snapshot, input Input) (Preview, error) {
	if strings.TrimSpace(input.Revision) == "" || input.Revision != snapshot.Revision {
		return Preview{}, ErrConflict
	}
	updates, err := s.normalizedUpdates(snapshot, input)
	if err != nil {
		return Preview{}, err
	}
	combined := make(map[string]string, len(fieldDefinitions))
	for _, definition := range fieldDefinitions {
		combined[definition.ID] = s.effectiveValue(definition.ID, snapshot.Values[definition.ID])
	}
	changes := make([]Change, 0, len(updates))
	for id, after := range updates {
		definition, _ := definitionByID(id)
		before := snapshot.Values[id]
		if before == after {
			continue
		}
		combined[id] = after
		change := Change{FieldID: id, Label: definition.Label, Before: before, After: after, Sensitive: definition.Sensitive}
		if definition.Sensitive {
			change.Before, change.After = configuredLabel(before), configuredLabel(after)
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].FieldID < changes[j].FieldID })
	issues := validateValues(combined)
	valid := true
	for _, issue := range issues {
		if issue.Severity == "error" {
			valid = false
			break
		}
	}
	restartRequired := false
	for _, change := range changes {
		definition, _ := definitionByID(change.FieldID)
		restartRequired = restartRequired || definition.RestartRequired && s.applier == nil
	}
	return Preview{Valid: valid, Revision: snapshot.Revision, Changes: changes, Issues: issues, RestartRequired: restartRequired}, nil
}

func (s *Service) normalizedUpdates(snapshot Snapshot, input Input) (map[string]string, error) {
	if len(input.Values) > len(fieldDefinitions) || len(input.ClearSecrets) > len(fieldDefinitions) {
		return nil, ErrInvalidInput
	}
	updates := make(map[string]string)
	for id, value := range input.Values {
		definition, ok := definitionByID(id)
		if !ok || !s.editable(definition) {
			return nil, ErrInvalidInput
		}
		value = strings.TrimSpace(value)
		if definition.Sensitive && value == "" {
			continue
		}
		if len(value) > 4096 {
			return nil, ErrInvalidInput
		}
		updates[id] = value
	}
	for _, id := range input.ClearSecrets {
		definition, ok := definitionByID(id)
		if !ok || !definition.Sensitive || !s.editable(definition) {
			return nil, ErrInvalidInput
		}
		updates[id] = ""
	}
	return updates, nil
}

func (s *Service) settingsFromSnapshot(snapshot Snapshot) Settings {
	fields := make([]Field, 0, len(fieldDefinitions))
	for _, definition := range fieldDefinitions {
		raw := snapshot.Values[definition.ID]
		value, source, environment := raw, SourceFile, ""
		if environmentValue, exists := s.lookupEnv(definition.Environment); exists && strings.TrimSpace(environmentValue) != "" {
			value, source, environment = strings.TrimSpace(environmentValue), SourceEnvironment, definition.Environment
		} else if strings.TrimSpace(value) == "" && definition.Default != "" {
			value, source = definition.Default, SourceDefault
		}
		configured := strings.TrimSpace(value) != ""
		if definition.Sensitive {
			value = ""
		}
		options := make([]string, len(definition.Options))
		copy(options, definition.Options)
		fields = append(fields, Field{ID: definition.ID, Group: definition.Group, Label: definition.Label, Kind: definition.Kind, Value: value, Options: options, Source: source, Environment: environment, Editable: source != SourceEnvironment && !definition.ReadOnly, Sensitive: definition.Sensitive, Configured: configured, RestartRequired: definition.RestartRequired && s.applier == nil, Minimum: definition.Minimum, Maximum: definition.Maximum})
	}
	restartRequired := s.runtimeChanged(snapshot.Values)
	return Settings{RuntimeApplySupported: s.applier != nil, Revision: snapshot.Revision, ConfigurationPath: snapshot.ConfigurationPath, BackupPath: snapshot.BackupPath, RestartRequired: restartRequired, Fields: fields, ReadAt: s.now().UTC()}
}

func (s *Service) editable(definition fieldDefinition) bool {
	if definition.ReadOnly {
		return false
	}
	value, exists := s.lookupEnv(definition.Environment)
	return !exists || strings.TrimSpace(value) == ""
}

func (s *Service) Runtime() (RuntimeSettings, error) {
	snapshot, err := s.repository.Snapshot()
	if err != nil {
		return RuntimeSettings{}, err
	}
	value := func(id string) string { return s.effectiveValue(id, snapshot.Values[id]) }
	return RuntimeSettings{
		SystemName: value("ui.systemName"), AdminEmail: value("ui.adminEmail"), Language: value("ui.language"),
		Timezone: value("ui.timezone"), DateFormat: value("ui.dateFormat"), Theme: value("ui.theme"),
		PasswordComplexity: parseBool(value("security.passwordComplexity")), MinPasswordLength: parseInt(value("security.minPasswordLength"), 6),
		SessionTimeout:   time.Duration(parseInt(value("security.sessionTimeout"), 1440)) * time.Minute,
		MaxLoginAttempts: parseInt(value("security.maxLoginAttempts"), 5), IPWhitelist: value("security.ipWhitelist"),
		AutoBackup: parseBool(value("backup.auto")), BackupFrequency: value("backup.frequency"), BackupTime: value("backup.time"),
		BackupRetention: parseInt(value("backup.retention"), 7), EmailEnabled: parseBool(value("notification.emailEnabled")),
		SMTPServer: value("notification.smtpServer"), SMTPPort: parseInt(value("notification.smtpPort"), 587),
		SMTPUsername: value("notification.smtpUsername"), SMTPPassword: value("notification.smtpPassword"), SenderEmail: value("notification.senderEmail"),
		NotifyServerStatus: parseBool(value("notification.serverStatus")), NotifyLoginFailures: parseBool(value("notification.loginFailures")),
		NotifyBackupResults: parseBool(value("notification.backupResults")), NotifySystemUpdates: parseBool(value("notification.systemUpdates")),
	}, nil
}

func (s *Service) effectiveValue(id, raw string) string {
	definition, _ := definitionByID(id)
	if value, exists := s.lookupEnv(definition.Environment); exists && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if strings.TrimSpace(raw) == "" {
		return definition.Default
	}
	return raw
}

func validateValues(values map[string]string) []Issue {
	issues := make([]Issue, 0)
	for _, definition := range fieldDefinitions {
		value := strings.TrimSpace(values[definition.ID])
		if definition.Required && value == "" {
			issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "不能为空"})
			continue
		}
		if value == "" {
			continue
		}
		switch definition.Kind {
		case "path":
			if !filepath.IsAbs(value) {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "必须使用绝对路径"})
			} else if _, err := os.Stat(value); err != nil {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "warning", Message: definition.Label + "当前不存在，重启前请确认路径"})
			}
		case "select":
			if !contains(definition.Options, value) {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "不是允许值"})
			}
		case "boolean":
			if value != "true" && value != "false" {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "必须为 true 或 false"})
			}
		case "number":
			number, err := strconv.Atoi(value)
			if err != nil || definition.Minimum != 0 && number < definition.Minimum || definition.Maximum != 0 && number > definition.Maximum {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "超出允许范围"})
			}
		case "email":
			address, err := mail.ParseAddress(value)
			if err != nil || !strings.EqualFold(address.Address, value) {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "格式无效"})
			}
		case "color":
			if !colorPattern.MatchString(value) && !rgbaPattern.MatchString(value) {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "必须为十六进制颜色"})
			}
		case "time":
			if _, err := time.Parse("15:04", value); err != nil {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "必须使用 HH:mm 格式"})
			}
		case "ip-list":
			if err := ValidateIPWhitelist(value); err != nil {
				issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: err.Error()})
			}
		}
		if definition.ID == "mod.steamAppID" && !digitsPattern.MatchString(value) {
			issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: "Steam App ID 必须为数字"})
		}
		if definition.Sensitive && definition.ID != "notification.smtpPassword" && !secretPattern.MatchString(value) {
			issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "格式无效"})
		}
		if strings.ContainsRune(value, 0) {
			issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "包含无效字符"})
		}
	}
	if nestedPaths(values["paths.save"], values["paths.backup"]) {
		issues = append(issues, Issue{FieldID: "paths.backup", Severity: "error", Message: "存档目录与备份目录不能相同或互相嵌套"})
	}
	if parseBool(values["notification.emailEnabled"]) {
		for _, id := range []string{"notification.smtpServer", "notification.smtpPort", "notification.smtpUsername", "notification.smtpPassword", "notification.senderEmail", "ui.adminEmail"} {
			if strings.TrimSpace(values[id]) == "" {
				definition, _ := definitionByID(id)
				issues = append(issues, Issue{FieldID: id, Severity: "error", Message: definition.Label + "不能为空"})
			}
		}
	}
	if _, err := deploymentprofile.Resolve(deploymentprofile.Values{
		Packaging: values["deployment.packaging"], LocalExecutorEnabled: values["fleet.localExecutorEnabled"],
		ControllerEnabled: values["fleet.controllerEnabled"], MemberEnabled: values["fleet.memberEnabled"],
		ControllerURL: values["fleet.controllerUrl"], MemberKey: values["fleet.memberKey"],
	}); err != nil {
		var validation *deploymentprofile.ValidationError
		if errors.As(err, &validation) {
			issues = append(issues, Issue{FieldID: validation.Field, Severity: "error", Message: validation.Message})
		} else {
			issues = append(issues, Issue{Severity: "error", Message: err.Error()})
		}
	}
	return issues
}

func ValidateIPWhitelist(value string) error {
	for _, line := range strings.Fields(value) {
		if _, err := netip.ParsePrefix(line); err == nil {
			continue
		}
		if _, err := netip.ParseAddr(line); err != nil {
			return fmt.Errorf("IP 白名单包含无效地址：%s", line)
		}
	}
	return nil
}

func IPAllowed(whitelist, address string) bool {
	if strings.TrimSpace(whitelist) == "" {
		return true
	}
	client, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	for _, line := range strings.Fields(whitelist) {
		if prefix, err := netip.ParsePrefix(line); err == nil && prefix.Contains(client) {
			return true
		}
		if allowed, err := netip.ParseAddr(line); err == nil && allowed == client {
			return true
		}
	}
	return false
}

func parseBool(value string) bool {
	parsed, _ := strconv.ParseBool(strings.TrimSpace(value))
	return parsed
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}

func nestedPaths(first, second string) bool {
	first, second = filepath.Clean(strings.TrimSpace(first)), filepath.Clean(strings.TrimSpace(second))
	if first == "." || second == "." || first == "" || second == "" {
		return false
	}
	return containsPath(first, second) || containsPath(second, first)
}

func containsPath(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) || parent == child
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func configuredLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "未配置"
	}
	return "已配置"
}
