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
	"unicode"
	"unicode/utf8"

	"dont/internal/dsttime"
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
	rule        Rule
	pattern     *regexp.Regexp
	tailPattern *regexp.Regexp
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
	counts, snapshot, err := s.store.Counts(room.ID, filter.WorldID)
	if err != nil {
		return List{}, err
	}
	for _, value := range allTypes() {
		if _, exists := counts[value]; !exists {
			counts[value] = 0
		}
	}
	return List{
		Items: items, Total: total, Counts: counts, Limit: filter.Limit, Offset: filter.Offset,
		SnapshotState: snapshot.State, SnapshotUpdatedAt: snapshot.UpdatedAt, LastRefreshedAt: snapshot.LastRefreshedAt,
	}, nil
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
	entries, err := classifySnapshot(ctx, snapshot.Lines, world, rules, observedAt, snapshot.StartedAt)
	if err != nil {
		return RefreshResult{}, err
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

func (s *Service) ClearWorld(roomID, worldID string) (ClearResult, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return ClearResult{}, err
	}
	world, err := s.rooms.World(room.ID, strings.TrimSpace(worldID))
	if err != nil {
		return ClearResult{}, err
	}
	lock := s.lock(room.ID + "\x00" + world.ID)
	lock.Lock()
	defer lock.Unlock()
	clearedAt := s.now().UTC()
	deleted, err := s.store.ClearWorldSnapshot(room.ID, world.ID, clearedAt)
	if err != nil {
		return ClearResult{}, err
	}
	return ClearResult{RoomID: room.ID, WorldID: world.ID, Deleted: deleted, ClearedAt: clearedAt}, nil
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
	return s.store.CreateRule(Rule{ID: id, RoomID: room.ID, Name: input.Name, Description: input.Description, LogType: input.LogType, Pattern: input.Pattern, Regex: input.Regex, Enabled: input.Enabled, Priority: input.Priority, MatchMode: input.MatchMode, TailPattern: input.TailPattern})
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
	current.MatchMode, current.TailPattern = input.MatchMode, input.TailPattern
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
	compiled, err := compileRule(Rule{Pattern: rule.Pattern, Regex: rule.Regex, MatchMode: rule.MatchMode, TailPattern: rule.TailPattern})
	if err != nil {
		return RuleTestResult{}, err
	}
	lines := strings.Split(input.Sample, "\n")
	matched := len(lines) > 0 && matches(compiled, lines[0])
	content := ""
	if matched {
		content = stripSourcePrefixes(lines)
		if rule.MatchMode == MatchModeHeadTail {
			matched = false
			for _, line := range lines[1:] {
				if compiled.tailPattern.MatchString(line) {
					matched = true
					break
				}
			}
		}
	}
	return RuleTestResult{Matched: matched, Content: content}, nil
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
	rules, err := s.ensureRulesInitialized(roomID)
	if err != nil {
		return nil, err
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

func (s *Service) ensureRulesInitialized(roomID string) ([]Rule, error) {
	lock := s.lock(roomID + "\x00built-in-rules")
	lock.Lock()
	defer lock.Unlock()
	rules, err := s.store.Rules(roomID)
	if err != nil {
		return nil, err
	}
	existingIDs := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		existingIDs[rule.ID] = struct{}{}
	}
	missing := make([]Rule, 0)
	for _, rule := range defaultRules(roomID) {
		if _, exists := existingIDs[rule.ID]; !exists {
			missing = append(missing, rule)
		}
	}
	if len(missing) == 0 {
		return rules, nil
	}
	if _, err := s.store.CreateRules(missing); err != nil {
		return nil, err
	}
	return s.store.Rules(roomID)
}

func (s *Service) lock(key string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if s.locks[key] == nil {
		s.locks[key] = &sync.Mutex{}
	}
	return s.locks[key]
}

func classifySnapshot(ctx context.Context, lines []logstream.Line, world rooms.World, rules []compiledRule, observedAt, startedAt time.Time) ([]Entry, error) {
	entries := make([]Entry, 0, len(lines))
	startTime := startedAt
	if startTime.IsZero() {
		startTime = snapshotStartTime(lines)
	}
	for index := 0; index < len(lines); index++ {
		if index%100 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if isStructuredLogNoise(lines[index].Text) {
			continue
		}
		entry, rule := classifyLine(lines[index], world, rules, observedAt, startTime)
		if rule == nil || rule.rule.MatchMode == MatchModeSingle {
			entries = append(entries, entry)
			continue
		}
		rawLines := []string{entry.RawContent}
		contentLines := []string{entry.Content}
		switch rule.rule.MatchMode {
		case MatchModeMultiLine:
			for next := index + 1; next < len(lines); next++ {
				if strings.TrimSpace(lines[next].Text) == "" {
					break
				}
				nextEntry, nextRule := classifyLine(lines[next], world, rules, observedAt, startTime)
				if nextRule == nil || nextRule.rule.LogType != rule.rule.LogType {
					break
				}
				rawLines = append(rawLines, nextEntry.RawContent)
				contentLines = append(contentLines, nextEntry.Content)
				index = next
			}
		case MatchModeHeadTail:
			for next := index + 1; next < len(lines); next++ {
				if strings.TrimSpace(lines[next].Text) == "" {
					continue
				}
				rawLines = append(rawLines, lines[next].Text)
				contentLines = append(contentLines, stripSourcePrefix(lines[next].Text))
				index = next
				if rule.tailPattern.MatchString(lines[next].Text) {
					break
				}
			}
		}
		entry.RawContent = strings.Join(rawLines, "\n")
		entry.Content = strings.Join(contentLines, "\n")
		entries = append(entries, entry)
	}
	return entries, nil
}

func snapshotStartTime(lines []logstream.Line) time.Time {
	var content strings.Builder
	for _, line := range lines {
		content.WriteString(line.Text)
		content.WriteByte('\n')
	}
	latest, _ := dsttime.FindStartTime(content.String())
	return latest
}

func classifyLine(line logstream.Line, world rooms.World, rules []compiledRule, observedAt time.Time, startTime time.Time) (Entry, *compiledRule) {
	entry := Entry{RoomID: world.RoomID, WorldID: world.ID, WorldName: world.Name, Type: TypeUnknown, Content: stripSourcePrefix(line.Text), RawContent: line.Text, SourceCursor: line.Cursor, ObservedAt: observedAt}
	if match := sourcePrefix.FindStringSubmatch(line.Text); len(match) == 2 {
		entry.SourceTimestamp = match[1]
		if value, err := time.Parse(time.RFC3339, match[1]); err == nil {
			value = value.UTC()
			entry.OccurredAt = &value
		} else if value, err := dsttime.ResolveTimestamp(startTime, match[1]); err == nil {
			value = value.UTC()
			entry.OccurredAt = &value
		}
	}
	for index := range rules {
		if matches(rules[index], line.Text) {
			entry.Type, entry.RuleID, entry.RuleName = rules[index].rule.LogType, rules[index].rule.ID, rules[index].rule.Name
			return entry, &rules[index]
		}
	}
	return entry, nil
}

func stripSourcePrefix(value string) string {
	return strings.TrimSpace(sourcePrefix.ReplaceAllString(strings.TrimSpace(value), ""))
}

func stripSourcePrefixes(lines []string) string {
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		values = append(values, stripSourcePrefix(line))
	}
	return strings.Join(values, "\n")
}

func isStructuredLogNoise(value string) bool {
	content := strings.TrimSpace(stripSourcePrefix(value))
	return content == "" || strings.Trim(content, "#") == ""
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
	if rule.MatchMode == MatchModeHeadTail {
		pattern, err := regexp.Compile(rule.TailPattern)
		if err != nil {
			return compiledRule{}, &FieldError{Fields: map[string]string{"tailPattern": "尾行正则表达式无效：" + err.Error()}}
		}
		compiled.tailPattern = pattern
	}
	return compiled, nil
}

func normalizeRule(input RuleInput) (RuleInput, error) {
	input.Name, input.Description, input.Pattern = strings.TrimSpace(input.Name), strings.TrimSpace(input.Description), strings.TrimSpace(input.Pattern)
	input.TailPattern = strings.TrimSpace(input.TailPattern)
	if input.MatchMode == "" {
		input.MatchMode = MatchModeSingle
	}
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
	if input.MatchMode != MatchModeSingle && input.MatchMode != MatchModeMultiLine && input.MatchMode != MatchModeHeadTail {
		fields["matchMode"] = "匹配模式必须是 single、multi_line 或 head_tail"
	}
	if input.MatchMode == MatchModeHeadTail {
		if input.TailPattern == "" || len(input.TailPattern) > 512 || !utf8.ValidString(input.TailPattern) || strings.ContainsRune(input.TailPattern, '\x00') {
			fields["tailPattern"] = "首尾行匹配必须提供 1-512 字节的有效尾行正则"
		} else if _, err := regexp.Compile(input.TailPattern); err != nil {
			fields["tailPattern"] = "尾行正则表达式无效：" + err.Error()
		}
	} else {
		input.TailPattern = ""
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
	runes := []rune(strings.TrimSpace(string(value)))
	if len(runes) == 0 || len(runes) > 48 {
		return false
	}
	for _, value := range runes {
		if !unicode.IsLetter(value) && !unicode.IsDigit(value) && value != '_' && value != '-' && value != '.' {
			return false
		}
	}
	return true
}

func allTypes() []LogType {
	return []LogType{
		TypeSystem, TypeChat, TypePlayer, TypeEntity, TypeWorld, TypeError, TypeWarning,
		TypeStartup, TypeWorldGen, TypeDiagnostic, TypeUnknown,
	}
}

func defaultRules(roomID string) []Rule {
	return []Rule{
		{ID: "builtin-error", RoomID: roomID, Name: "错误", Description: "运行错误、断言和堆栈", LogType: TypeError, Pattern: `(?i)\b(error|assert|panic|stack traceback)\b`, Regex: true, Enabled: true, Priority: 900, BuiltIn: true},
		{ID: "builtin-worldgen-warning", RoomID: roomID, Name: "世界生成异常", Description: "世界生成失败、迷宫生成中止等可恢复异常", LogType: TypeWarning, Pattern: `(?i)((?:world|wold)gen failed|couldn't generate.*aborting|poly\.size\(\) == 0|\[!\].*edge == null)`, Regex: true, Enabled: true, Priority: 890, BuiltIn: true},
		{ID: "builtin-warning", RoomID: roomID, Name: "警告", Description: "警告和弃用提示", LogType: TypeWarning, Pattern: `(?i)\b(warn(?:ing)?|deprecated)\b`, Regex: true, Enabled: true, Priority: 800, BuiltIn: true},
		{ID: "builtin-chat", RoomID: roomID, Name: "聊天", Description: "玩家聊天消息", LogType: TypeChat, Pattern: `(?i)\b(say|chat)\s*[\(:]`, Regex: true, Enabled: true, Priority: 700, BuiltIn: true},
		{ID: "builtin-startup", RoomID: roomID, Name: "专服启动", Description: "专服路径、构建版本、平台与 Lua 初始化", LogType: TypeStartup, Pattern: `(?i)(persistrootstorage|current time:|don't starve together:|build date:|parsing command line|initializing distribution platform|curlrequestmanager|profileindex|onload(?:permission|userid)list|token retrieved from|(?:renderer|animmanager|buffers|gamespecific) initialize|cgame::|appversion|getarchitecture|loading lua|doluafile|running main\.lua|loaded modoverrides|registering mods|no mods registered|onfilesloaded|filesexist|check for (?:write|read) access)`, Regex: true, Enabled: true, Priority: 680, MatchMode: MatchModeMultiLine, BuiltIn: true},
		{ID: "builtin-startup-detail", RoomID: roomID, Name: "启动细节", Description: "前端资源、存档覆盖和堆栈模块等启动步骤", LogType: TypeStartup, Pattern: `(?i)(^\[\d{2}:\d{2}:\d{2}\]:\s+\.{4}done|\[connect\] pendingconnection|platform:\s*\d+|dontstarvegame::|load fe|reset\(\) returning|level data override|loaded and applied level data override|overwriting savedata|install(?:ed)? stacktrace)`, Regex: true, Enabled: true, Priority: 675, MatchMode: MatchModeMultiLine, BuiltIn: true},
		{ID: "builtin-server-config", RoomID: roomID, Name: "专服参数", Description: "端口、人数、模式和联网等启动参数", LogType: TypeStartup, Pattern: `(?i)^\[\d{2}:\d{2}:\d{2}\]:\s+(dedicated|online|passworded|serverport|steamauthport|steammasterserverport|clanid|clanonly|clanadmin|lanonly|friendsonly|enableautosaver|encodeuserpath|pvp|maxplayers|gamemode|overridendns|pausewhenempty|idletimeout|voteenabled|internetbroadcasting):`, Regex: true, Enabled: true, Priority: 670, MatchMode: MatchModeMultiLine, BuiltIn: true},
		{ID: "builtin-worldgen", RoomID: roomID, Name: "世界生成", Description: "地形、海洋、道路、节点和预制物生成过程", LogType: TypeWorldGen, Pattern: `(?i)(worldgen|worldsim|story gen|boostmap|voronoi|landmass|tilemap|separateislands|drawroads|replace.*tiles|\[ocean\]|creating story|baking map|map baked|checking required prefab|prefab swap|finding valid start task|has start node|adding background nodes|populating voronoi|checking tags|\btag:|disconnected tiles|removing entity on impassable|encoding)`, Regex: true, Enabled: true, Priority: 650, MatchMode: MatchModeMultiLine, BuiltIn: true},
		{ID: "builtin-worldgen-progress", RoomID: roomID, Name: "世界生成进度", Description: "种子、地图尺寸、任务布局和生成完成状态", LogType: TypeWorldGen, Pattern: `(?i)(generating .* mode level|engine seed:|^\[\d{2}:\d{2}:\d{2}\]:\s+(seed\s*=|level_type|level_data:|dlc enabled|new size:)|\.{3}\s*(done\.?|story created|picked)|detectdisconnect|done (?:cave|forest) map gen|checking map|added to task)`, Regex: true, Enabled: true, Priority: 645, MatchMode: MatchModeMultiLine, BuiltIn: true},
		{ID: "builtin-player", RoomID: roomID, Name: "玩家事件", Description: "玩家加入、离开和管理动作", LogType: TypePlayer, Pattern: `(?i)\b(player|client).*(joined|left|connected|disconnected|kicked|banned)\b`, Regex: true, Enabled: true, Priority: 600, BuiltIn: true},
		{ID: "builtin-world", RoomID: roomID, Name: "世界事件", Description: "世界、季节和分片事件", LogType: TypeWorld, Pattern: `(?i)\b(world|season|phase|cycle|shard)\b`, Regex: true, Enabled: true, Priority: 500, BuiltIn: true},
		{ID: "builtin-diagnostic", RoomID: roomID, Name: "引擎诊断", Description: "Lua 回收、资源释放和可选本地数据加载状态", LogType: TypeDiagnostic, Pattern: `(?i)(collecting garbage|lua_gc took|lua_close took|releaseall|~(?:shardlua|ceventleaderboard|itemserverlua|inventorylua|networklua|simlua)proxy|event data unavailable|playerdeaths could not load|playerhistory could not load|serverpreferences could not load|consolescreensettings could not load|bloom_enabled|onupdatepurchasestatecomplete|klump files loaded)`, Regex: true, Enabled: true, Priority: 200, MatchMode: MatchModeMultiLine, BuiltIn: true},
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
