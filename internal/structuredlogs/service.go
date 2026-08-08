package structuredlogs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dont/internal/logstream"
	"dont/internal/rooms"
)

const maxRuleSampleBytes = 64 * 1024

var sourcePrefix = regexp.MustCompile(`^\[([^\]]+)\][:\s]*`)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
	Worlds(string) ([]rooms.World, error)
}

type RawLogs interface {
	Snapshot(string, string, int, string) (logstream.Snapshot, error)
}

type compiledRule struct {
	rule    Rule
	pattern *regexp.Regexp
}

type Service struct {
	rooms   RoomCatalog
	logs    RawLogs
	store   *Store
	now     func() time.Time
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewService(roomCatalog RoomCatalog, rawLogs RawLogs, store *Store) (*Service, error) {
	if roomCatalog == nil || rawLogs == nil || store == nil {
		return nil, errors.New("rooms, raw logs, and structured log store are required")
	}
	return &Service{rooms: roomCatalog, logs: rawLogs, store: store, now: time.Now, locks: make(map[string]*sync.Mutex)}, nil
}

func (s *Service) List(roomID string, filter ListFilter) (List, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return List{}, err
	}
	filter, err = s.normalizeFilter(room.ID, filter)
	if err != nil {
		return List{}, err
	}
	items, total, err := s.store.List(room.ID, filter)
	if err != nil {
		return List{}, err
	}
	counts, refreshed, err := s.store.Counts(room.ID)
	if err != nil {
		return List{}, err
	}
	for _, value := range allTypes() {
		if _, exists := counts[value]; !exists {
			counts[value] = 0
		}
	}
	return List{Items: items, Total: total, Counts: counts, Limit: filter.Limit, Offset: filter.Offset, LastRefreshedAt: refreshed}, nil
}

func (s *Service) WorldTargets(roomID string) ([]rooms.World, error) {
	if _, err := s.managedRoom(roomID); err != nil {
		return nil, err
	}
	return s.rooms.Worlds(roomID)
}

func (s *Service) RefreshWorld(ctx context.Context, roomID, worldID string) (RefreshResult, error) {
	if err := ctx.Err(); err != nil {
		return RefreshResult{}, err
	}
	lock := s.lock(roomID + "\x00" + worldID)
	lock.Lock()
	defer lock.Unlock()
	room, err := s.managedRoom(roomID)
	if err != nil {
		return RefreshResult{}, err
	}
	world, err := s.rooms.World(room.ID, worldID)
	if err != nil {
		return RefreshResult{}, err
	}
	snapshot, err := s.logs.Snapshot(room.ID, world.ID, 2000, "")
	if err != nil {
		return RefreshResult{}, err
	}
	rules, err := s.rules(room.ID)
	if err != nil {
		return RefreshResult{}, err
	}
	observedAt := s.now().UTC()
	entries := make([]Entry, 0, len(snapshot.Lines))
	for index, line := range snapshot.Lines {
		if index%100 == 0 {
			if err := ctx.Err(); err != nil {
				return RefreshResult{}, err
			}
		}
		entries = append(entries, classify(line, world, rules, observedAt))
	}
	if err := s.store.ReplaceWorldSnapshot(room.ID, world.ID, world.Name, entries, observedAt); err != nil {
		return RefreshResult{}, err
	}
	message := fmt.Sprintf("已解析 %d 行日志", len(entries))
	if snapshot.Truncated {
		message += "（仅保留受限尾部快照）"
	}
	return RefreshResult{WorldID: world.ID, Count: len(entries), Truncated: snapshot.Truncated, Message: message}, nil
}

func (s *Service) Rules(roomID string) ([]Rule, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return nil, err
	}
	if _, err := s.rules(room.ID); err != nil {
		return nil, err
	}
	return s.store.Rules(room.ID)
}

func (s *Service) CreateRule(roomID string, input RuleInput) (Rule, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return Rule{}, err
	}
	input, err = normalizeRule(input)
	if err != nil {
		return Rule{}, err
	}
	id, err := randomID()
	if err != nil {
		return Rule{}, err
	}
	return s.store.CreateRule(Rule{ID: id, RoomID: room.ID, Name: input.Name, Description: input.Description, LogType: input.LogType, Pattern: input.Pattern, Regex: input.Regex, Enabled: input.Enabled, Priority: input.Priority})
}

func (s *Service) UpdateRule(roomID, ruleID string, input RuleInput) (Rule, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return Rule{}, err
	}
	input, err = normalizeRule(input)
	if err != nil {
		return Rule{}, err
	}
	current, err := s.store.Rule(room.ID, strings.TrimSpace(ruleID))
	if err != nil {
		return Rule{}, err
	}
	current.Name, current.Description, current.LogType = input.Name, input.Description, input.LogType
	current.Pattern, current.Regex, current.Enabled, current.Priority = input.Pattern, input.Regex, input.Enabled, input.Priority
	return s.store.UpdateRule(current)
}

func (s *Service) DeleteRule(roomID, ruleID string) error {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return err
	}
	return s.store.DeleteRule(room.ID, strings.TrimSpace(ruleID))
}

func (s *Service) TestRule(roomID string, input RuleTestInput) (RuleTestResult, error) {
	if _, err := s.managedRoom(roomID); err != nil {
		return RuleTestResult{}, err
	}
	if len(input.Sample) > maxRuleSampleBytes || !utf8.ValidString(input.Sample) || strings.ContainsRune(input.Sample, '\x00') {
		return RuleTestResult{}, &FieldError{Fields: map[string]string{"sample": "样本必须是不超过 64 KiB 的有效文本"}}
	}
	rule, err := normalizeRule(input.RuleInput)
	if err != nil {
		return RuleTestResult{}, err
	}
	compiled, err := compileRule(Rule{Pattern: rule.Pattern, Regex: rule.Regex})
	if err != nil {
		return RuleTestResult{}, err
	}
	return RuleTestResult{Matched: matches(compiled, input.Sample), Content: stripSourcePrefix(input.Sample)}, nil
}

func (s *Service) managedRoom(roomID string) (rooms.Room, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, err
	}
	if !room.Managed {
		return rooms.Room{}, ErrRoomNotManaged
	}
	return room, nil
}

func (s *Service) normalizeFilter(roomID string, filter ListFilter) (ListFilter, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	filter.WorldID = strings.TrimSpace(filter.WorldID)
	if len([]rune(filter.Query)) > 256 || (filter.Type != "" && !validType(filter.Type)) {
		return ListFilter{}, ErrInvalidFilter
	}
	if filter.WorldID != "" {
		if _, err := s.rooms.World(roomID, filter.WorldID); err != nil {
			return ListFilter{}, err
		}
	}
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 50
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	return filter, nil
}

func (s *Service) rules(roomID string) ([]compiledRule, error) {
	rules, err := s.store.Rules(roomID)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		for _, rule := range defaultRules(roomID) {
			if _, err := s.store.CreateRule(rule); err != nil {
				return nil, err
			}
		}
		rules, err = s.store.Rules(roomID)
		if err != nil {
			return nil, err
		}
	}
	compiled := make([]compiledRule, 0, len(rules))
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		value, compileErr := compileRule(rule)
		if compileErr != nil {
			return nil, compileErr
		}
		compiled = append(compiled, value)
	}
	sort.SliceStable(compiled, func(i, j int) bool { return compiled[i].rule.Priority > compiled[j].rule.Priority })
	return compiled, nil
}

func (s *Service) lock(key string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if s.locks[key] == nil {
		s.locks[key] = &sync.Mutex{}
	}
	return s.locks[key]
}

func classify(line logstream.Line, world rooms.World, rules []compiledRule, observedAt time.Time) Entry {
	entry := Entry{RoomID: world.RoomID, WorldID: world.ID, WorldName: world.Name, Type: TypeUnknown, Content: stripSourcePrefix(line.Text), RawContent: line.Text, SourceCursor: line.Cursor, ObservedAt: observedAt}
	if match := sourcePrefix.FindStringSubmatch(line.Text); len(match) == 2 {
		entry.SourceTimestamp = match[1]
		if value, err := time.Parse(time.RFC3339, match[1]); err == nil {
			value = value.UTC()
			entry.OccurredAt = &value
		}
	}
	for _, rule := range rules {
		if matches(rule, line.Text) {
			entry.Type, entry.RuleID, entry.RuleName = rule.rule.LogType, rule.rule.ID, rule.rule.Name
			break
		}
	}
	return entry
}

func stripSourcePrefix(value string) string {
	return strings.TrimSpace(sourcePrefix.ReplaceAllString(strings.TrimSpace(value), ""))
}

func matches(rule compiledRule, sample string) bool {
	if rule.pattern != nil {
		return rule.pattern.MatchString(sample)
	}
	return strings.Contains(strings.ToLower(sample), strings.ToLower(rule.rule.Pattern))
}

func compileRule(rule Rule) (compiledRule, error) {
	compiled := compiledRule{rule: rule}
	if rule.Regex {
		pattern, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return compiledRule{}, &FieldError{Fields: map[string]string{"pattern": "正则表达式无效：" + err.Error()}}
		}
		compiled.pattern = pattern
	}
	return compiled, nil
}

func normalizeRule(input RuleInput) (RuleInput, error) {
	input.Name, input.Description, input.Pattern = strings.TrimSpace(input.Name), strings.TrimSpace(input.Description), strings.TrimSpace(input.Pattern)
	fields := make(map[string]string)
	if input.Name == "" || len([]rune(input.Name)) > 80 {
		fields["name"] = "名称必须为 1-80 个字符"
	}
	if len([]rune(input.Description)) > 300 {
		fields["description"] = "说明不能超过 300 个字符"
	}
	if !validType(input.LogType) {
		fields["logType"] = "日志类型无效"
	}
	if input.Pattern == "" || len(input.Pattern) > 512 || !utf8.ValidString(input.Pattern) || strings.ContainsRune(input.Pattern, '\x00') {
		fields["pattern"] = "模式必须为 1-512 字节的有效文本"
	}
	if input.Priority < 0 || input.Priority > 1000 {
		fields["priority"] = "优先级必须为 0-1000"
	}
	if len(fields) == 0 && input.Regex {
		if _, err := regexp.Compile(input.Pattern); err != nil {
			fields["pattern"] = "正则表达式无效：" + err.Error()
		}
	}
	if len(fields) > 0 {
		return RuleInput{}, &FieldError{Fields: fields}
	}
	return input, nil
}

func validType(value LogType) bool {
	for _, item := range allTypes() {
		if item == value {
			return true
		}
	}
	return false
}

func allTypes() []LogType {
	return []LogType{TypeSystem, TypeChat, TypePlayer, TypeEntity, TypeWorld, TypeError, TypeWarning, TypeUnknown}
}

func defaultRules(roomID string) []Rule {
	return []Rule{
		{ID: "builtin-error", RoomID: roomID, Name: "错误", Description: "运行错误、断言和堆栈", LogType: TypeError, Pattern: `(?i)\b(error|assert|panic|stack traceback)\b`, Regex: true, Enabled: true, Priority: 900, BuiltIn: true},
		{ID: "builtin-warning", RoomID: roomID, Name: "警告", Description: "警告和弃用提示", LogType: TypeWarning, Pattern: `(?i)\b(warn(?:ing)?|deprecated)\b`, Regex: true, Enabled: true, Priority: 800, BuiltIn: true},
		{ID: "builtin-chat", RoomID: roomID, Name: "聊天", Description: "玩家聊天消息", LogType: TypeChat, Pattern: `(?i)\b(say|chat)\s*[\(:]`, Regex: true, Enabled: true, Priority: 700, BuiltIn: true},
		{ID: "builtin-player", RoomID: roomID, Name: "玩家事件", Description: "玩家加入、离开和管理动作", LogType: TypePlayer, Pattern: `(?i)\b(player|client).*(joined|left|connected|disconnected|kicked|banned)\b`, Regex: true, Enabled: true, Priority: 600, BuiltIn: true},
		{ID: "builtin-world", RoomID: roomID, Name: "世界事件", Description: "世界、季节和分片事件", LogType: TypeWorld, Pattern: `(?i)\b(world|season|phase|cycle|shard)\b`, Regex: true, Enabled: true, Priority: 500, BuiltIn: true},
		{ID: "builtin-system", RoomID: roomID, Name: "系统", Description: "启动、版本、Steam 和 Mod 系统信息", LogType: TypeSystem, Pattern: `(?i)\b(starting|started|stopped|version|steam|modindex|server)\b`, Regex: true, Enabled: true, Priority: 100, BuiltIn: true},
	}
}

func randomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
