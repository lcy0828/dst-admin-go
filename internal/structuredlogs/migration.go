package structuredlogs

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type preparedRuleMigration struct {
	preview RuleMigrationPreview
	rules   []Rule
}

func (s *Service) PreviewRuleMigration(roomID string) (RuleMigrationPreview, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return RuleMigrationPreview{}, err
	}
	prepared, err := s.prepareRuleMigration(room.ID)
	if err != nil {
		return RuleMigrationPreview{}, err
	}
	return prepared.preview, nil
}

func (s *Service) MigrateLegacyRules(roomID string) (RuleMigrationResult, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return RuleMigrationResult{}, err
	}
	lock := s.lock(room.ID + "\x00rule-migration")
	lock.Lock()
	defer lock.Unlock()
	prepared, err := s.prepareRuleMigration(room.ID)
	if err != nil {
		return RuleMigrationResult{}, err
	}
	created, err := s.store.CreateRules(prepared.rules)
	if err != nil {
		return RuleMigrationResult{}, err
	}
	return RuleMigrationResult{Imported: len(created), Preview: prepared.preview}, nil
}

func (s *Service) prepareRuleMigration(roomID string) (preparedRuleMigration, error) {
	legacyRules, sourceAvailable, err := s.store.LegacyRules()
	if err != nil {
		return preparedRuleMigration{}, err
	}
	existing, err := s.ensureRulesInitialized(roomID)
	if err != nil {
		return preparedRuleMigration{}, err
	}
	byID := make(map[string]Rule, len(existing))
	byFingerprint := make(map[string]Rule, len(existing))
	byMatcher := make(map[string]Rule, len(existing))
	for _, rule := range existing {
		byID[rule.ID] = rule
		byFingerprint[ruleFingerprint(rule)] = rule
		byMatcher[ruleMatcherFingerprint(rule)] = rule
	}
	prepared := preparedRuleMigration{
		preview: RuleMigrationPreview{SourceAvailable: sourceAvailable, Total: len(legacyRules), Items: make([]RuleMigrationItem, 0, len(legacyRules))},
		rules:   make([]Rule, 0, len(legacyRules)),
	}
	for _, legacy := range legacyRules {
		item, rule := prepareLegacyRule(roomID, legacy)
		if item.Status == RuleMigrationReady {
			if current, ok := byID[rule.ID]; ok {
				item.ExistingRuleID = current.ID
				if ruleFingerprint(current) == ruleFingerprint(rule) {
					item.Status, item.Reason = RuleMigrationSkipped, "该旧版规则已经迁移"
				} else {
					item.Status, item.Reason = RuleMigrationConflict, "目标规则 ID 已存在且内容不同"
				}
			} else if current, ok := byFingerprint[ruleFingerprint(rule)]; ok {
				item.Status, item.Reason, item.ExistingRuleID = RuleMigrationSkipped, "已有等效规则，无需重复导入", current.ID
			} else if current, ok := byMatcher[ruleMatcherFingerprint(rule)]; ok {
				item.Status, item.Reason, item.ExistingRuleID = RuleMigrationConflict, "相同匹配条件已用于其他日志类型", current.ID
			}
		}
		if item.Status == RuleMigrationReady {
			prepared.rules = append(prepared.rules, rule)
			byID[rule.ID] = rule
			byFingerprint[ruleFingerprint(rule)] = rule
			byMatcher[ruleMatcherFingerprint(rule)] = rule
		}
		prepared.preview.Items = append(prepared.preview.Items, item)
		incrementMigrationCount(&prepared.preview, item.Status)
	}
	sort.SliceStable(prepared.preview.Items, func(i, j int) bool {
		left, right := prepared.preview.Items[i], prepared.preview.Items[j]
		if left.Status != right.Status {
			return migrationStatusOrder(left.Status) < migrationStatusOrder(right.Status)
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		return left.LegacyID < right.LegacyID
	})
	return prepared, nil
}

func prepareLegacyRule(roomID string, legacy legacyRuleRecord) (RuleMigrationItem, Rule) {
	matchMode := MatchMode(strings.TrimSpace(legacy.MatchMode))
	if matchMode == "" {
		matchMode = MatchModeSingle
	}
	priority := migratedPriority(legacy.Priority)
	targetID := "legacy-" + strconv.Itoa(legacy.ID)
	item := RuleMigrationItem{
		LegacyID: legacy.ID, Name: strings.TrimSpace(legacy.Name), LogType: LogType(strings.TrimSpace(legacy.LogType)),
		Pattern: strings.TrimSpace(legacy.Pattern), Regex: legacy.IsRegex, Enabled: legacy.IsEnabled,
		Priority: priority, MatchMode: string(matchMode), TailPattern: strings.TrimSpace(legacy.TailPattern),
		Status: RuleMigrationReady, Reason: "可以导入", TargetRuleID: targetID,
	}
	if legacyRuleMustBeSkipped(legacy) {
		item.Status = RuleMigrationSkipped
		item.Reason = "旧规则属于过宽兜底或包含已知错误，导入后会吞掉细分类"
		return item, Rule{}
	}
	if matchMode == MatchMode("fixed_lines") {
		item.Status = RuleMigrationIncompatible
		item.Reason = fmt.Sprintf("新版暂不支持 fixed_lines（旧规则配置为 %d 行）", legacy.LineCount)
		return item, Rule{}
	}
	input, err := normalizeRule(RuleInput{
		Name: item.Name, Description: migratedDescription(legacy.Description), LogType: item.LogType,
		Pattern: item.Pattern, Regex: item.Regex, Enabled: item.Enabled, Priority: item.Priority,
		MatchMode: matchMode, TailPattern: item.TailPattern,
	})
	if err != nil {
		item.Status = RuleMigrationIncompatible
		item.Reason = migrationValidationReason(err)
		return item, Rule{}
	}
	rule := Rule{
		ID: targetID, RoomID: roomID, Name: input.Name, Description: input.Description, LogType: input.LogType,
		Pattern: input.Pattern, Regex: input.Regex, Enabled: input.Enabled, Priority: input.Priority,
		MatchMode: input.MatchMode, TailPattern: input.TailPattern,
	}
	return item, rule
}

func legacyRuleMustBeSkipped(rule legacyRuleRecord) bool {
	name := strings.ToLower(strings.TrimSpace(rule.Name))
	pattern := strings.TrimSpace(rule.Pattern)
	if pattern == ".*" || name == "匹配所有日志" || name == "系统消息" {
		return true
	}
	return name == "dst-admin-go" && strings.Contains(pattern, "[DST-ADMIN-GO]")
}

func migratedPriority(priority int) int {
	if priority < 0 {
		priority = 0
	}
	if priority > 100 {
		priority = 100
	}
	return 500 + priority*4
}

func migratedDescription(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "从旧版日志解析器迁移"
	}
	const suffix = "（从旧版日志解析器迁移）"
	if len([]rune(value))+len([]rune(suffix)) <= 300 {
		return value + suffix
	}
	runes := []rune(value)
	return string(runes[:300-len([]rune(suffix))]) + suffix
}

func migrationValidationReason(err error) string {
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || len(fieldErr.Fields) == 0 {
		return "旧规则参数不受新版支持"
	}
	keys := make([]string, 0, len(fieldErr.Fields))
	for key := range fieldErr.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return fieldErr.Fields[keys[0]]
}

func ruleFingerprint(rule Rule) string {
	return strings.Join([]string{ruleMatcherFingerprint(rule), strings.ToLower(string(rule.LogType))}, "\x00")
}

func ruleMatcherFingerprint(rule Rule) string {
	return strings.Join([]string{
		strings.TrimSpace(rule.Pattern), strconv.FormatBool(rule.Regex), string(rule.MatchMode), strings.TrimSpace(rule.TailPattern),
	}, "\x00")
}

func incrementMigrationCount(preview *RuleMigrationPreview, status RuleMigrationStatus) {
	switch status {
	case RuleMigrationReady:
		preview.Ready++
	case RuleMigrationSkipped:
		preview.Skipped++
	case RuleMigrationConflict:
		preview.Conflicts++
	case RuleMigrationIncompatible:
		preview.Incompatible++
	}
}

func migrationStatusOrder(status RuleMigrationStatus) int {
	switch status {
	case RuleMigrationReady:
		return 0
	case RuleMigrationConflict:
		return 1
	case RuleMigrationIncompatible:
		return 2
	default:
		return 3
	}
}
