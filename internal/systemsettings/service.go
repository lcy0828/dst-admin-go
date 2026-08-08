package systemsettings

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var digitsPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var secretPattern = regexp.MustCompile(`^[A-Za-z0-9_+=./:-]{8,512}$`)

type Service struct {
	repository      Repository
	startupRevision string
	lookupEnv       func(string) (string, bool)
	now             func() time.Time
}

func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("system settings repository is required")
	}
	snapshot, err := repository.Snapshot()
	if err != nil {
		return nil, err
	}
	return &Service{repository: repository, startupRevision: snapshot.Revision, lookupEnv: os.LookupEnv, now: time.Now}, nil
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
	if len(updates) > 0 {
		snapshot, err = s.repository.Save(snapshot.Revision, updates)
		if err != nil {
			return ApplyResult{}, err
		}
	}
	return ApplyResult{Settings: s.settingsFromSnapshot(snapshot), Changes: preview.Changes}, nil
}

func (s *Service) preview(snapshot Snapshot, input Input) (Preview, error) {
	if strings.TrimSpace(input.Revision) == "" || input.Revision != snapshot.Revision {
		return Preview{}, ErrConflict
	}
	updates, err := s.normalizedUpdates(snapshot, input)
	if err != nil {
		return Preview{}, err
	}
	combined := make(map[string]string, len(snapshot.Values))
	for id, value := range snapshot.Values {
		combined[id] = s.effectiveValue(id, value)
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
	return Preview{Valid: valid, Revision: snapshot.Revision, Changes: changes, Issues: issues, RestartRequired: len(changes) > 0}, nil
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
		fields = append(fields, Field{ID: definition.ID, Group: definition.Group, Label: definition.Label, Kind: definition.Kind, Value: value, Options: options, Source: source, Environment: environment, Editable: source != SourceEnvironment, Sensitive: definition.Sensitive, Configured: configured, RestartRequired: true})
	}
	return Settings{Revision: snapshot.Revision, ConfigurationPath: snapshot.ConfigurationPath, BackupPath: snapshot.BackupPath, RestartRequired: snapshot.Revision != s.startupRevision, Fields: fields, ReadAt: s.now().UTC()}
}

func (s *Service) editable(definition fieldDefinition) bool {
	value, exists := s.lookupEnv(definition.Environment)
	return !exists || strings.TrimSpace(value) == ""
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
		}
		if definition.ID == "mod.steamAppID" && !digitsPattern.MatchString(value) {
			issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: "Steam App ID 必须为数字"})
		}
		if definition.Sensitive && !secretPattern.MatchString(value) {
			issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "格式无效"})
		}
		if strings.ContainsRune(value, 0) {
			issues = append(issues, Issue{FieldID: definition.ID, Severity: "error", Message: definition.Label + "包含无效字符"})
		}
	}
	if nestedPaths(values["paths.save"], values["paths.backup"]) {
		issues = append(issues, Issue{FieldID: "paths.backup", Severity: "error", Message: "存档目录与备份目录不能相同或互相嵌套"})
	}
	return issues
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
