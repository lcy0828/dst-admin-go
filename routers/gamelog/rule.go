package gamelog

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// 规则类型常量
const (
	RuleTypeInclude   = "include"   // 包含规则（只显示匹配的内容）
	RuleTypeExclude   = "exclude"   // 排除规则（不显示匹配的内容）
	RuleTypeHighlight = "highlight" // 高亮规则（高亮显示匹配的内容）
	RuleTypeReplace   = "replace"   // 替换规则（将匹配的内容替换为其他内容）
	RuleTypeGroup     = "group"     // 组合规则（多条件匹配）
)

// 规则匹配模式常量
const (
	MatchModeContains    = "contains"    // 包含字符串
	MatchModeNotContains = "notcontains" // 不包含字符串
	MatchModeRegex       = "regex"       // 正则表达式
	MatchModePrefix      = "prefix"      // 前缀匹配
	MatchModeSuffix      = "suffix"      // 后缀匹配
	MatchModeEquals      = "equals"      // 完全匹配
	MatchModeNotEquals   = "notequals"   // 不完全匹配
)

// LogRule 表示一个日志过滤规则
type LogRule struct {
	ID          string    `json:"id"`          // 规则ID
	Name        string    `json:"name"`        // 规则名称
	Type        string    `json:"type"`        // 规则类型：include, exclude, highlight, replace, group
	MatchMode   string    `json:"match_mode"`  // 匹配模式：contains, notcontains, regex, prefix, suffix, equals, notequals
	Pattern     string    `json:"pattern"`     // 匹配模式
	Color       string    `json:"color"`       // 高亮颜色（仅当type为highlight时有效）
	Replacement string    `json:"replacement"` // 替换内容（仅当type为replace时有效）
	Priority    int       `json:"priority"`    // 规则优先级（0-100，越高越先执行）
	Group       string    `json:"group"`       // 规则分组
	ChildRules  []string  `json:"child_rules"` // 子规则ID列表（仅当type为group时有效）
	GroupMode   string    `json:"group_mode"`  // 组合模式：and(所有子规则都匹配), or(任一子规则匹配)
	Enabled     bool      `json:"enabled"`     // 是否启用
	Description string    `json:"description"` // 规则描述
	MatchCount  int64     `json:"match_count"` // 匹配次数
	CreatedAt   time.Time `json:"created_at"`  // 创建时间
	UpdatedAt   time.Time `json:"updated_at"`  // 更新时间

	// 编译后的正则表达式（内部使用，不序列化）
	compiledRegex *regexp.Regexp `json:"-"`
}

// 规则组合模式常量
const (
	GroupModeAnd = "and" // 所有子规则都匹配
	GroupModeOr  = "or"  // 任一子规则匹配
)

// RuleGroup 表示一个规则分组
type RuleGroup struct {
	ID          string    `json:"id"`          // 分组ID
	Name        string    `json:"name"`        // 分组名称
	Description string    `json:"description"` // 分组描述
	Enabled     bool      `json:"enabled"`     // 是否启用
	CreatedAt   time.Time `json:"created_at"`  // 创建时间
	UpdatedAt   time.Time `json:"updated_at"`  // 更新时间
}

// RuleStats 表示规则统计信息
type RuleStats struct {
	TotalMatches int64            `json:"total_matches"` // 总匹配次数
	RuleMatches  map[string]int64 `json:"rule_matches"`  // 各规则匹配次数
	GroupMatches map[string]int64 `json:"group_matches"` // 各分组匹配次数
	LastUpdated  time.Time        `json:"last_updated"`  // 最后更新时间
}

// RuleManager 管理日志过滤规则
type RuleManager struct {
	Rules       map[string]*LogRule   `json:"rules"`        // 规则映射，键为规则ID
	Groups      map[string]*RuleGroup `json:"groups"`       // 规则分组，键为分组ID
	Stats       RuleStats             `json:"stats"`        // 规则统计信息
	ArchiveName string                `json:"archive_name"` // 存档名称
	WorldName   string                `json:"world_name"`   // 世界名称

	mutex     sync.RWMutex // 保护并发访问的互斥锁
	rulesFile string       // 规则文件路径
	statsFile string       // 统计文件路径
}

// 全局规则管理器映射
var ruleManagers = make(map[string]*RuleManager)
var ruleManagersMutex sync.RWMutex

// 获取或创建规则管理器
func GetOrCreateRuleManager(archiveName, worldName string) *RuleManager {
	ruleManagersMutex.Lock()
	defer ruleManagersMutex.Unlock()

	key := fmt.Sprintf("%s_%s", archiveName, worldName)
	if manager, ok := ruleManagers[key]; ok {
		return manager
	}

	// 创建规则目录
	rulesDir := "./conf/rules"
	if err := os.MkdirAll(rulesDir, 0755); err != nil {
		log.Printf("创建规则目录失败: %v", err)
	}

	// 规则文件路径
	rulesFile := filepath.Join(rulesDir, fmt.Sprintf("%s.json", key))

	// 统计文件路径
	statsFile := filepath.Join(rulesDir, fmt.Sprintf("%s_stats.json", key))

	// 创建新的规则管理器
	manager := &RuleManager{
		Rules:  make(map[string]*LogRule),
		Groups: make(map[string]*RuleGroup),
		Stats: RuleStats{
			TotalMatches: 0,
			RuleMatches:  make(map[string]int64),
			GroupMatches: make(map[string]int64),
			LastUpdated:  time.Now(),
		},
		ArchiveName: archiveName,
		WorldName:   worldName,
		rulesFile:   rulesFile,
		statsFile:   statsFile,
	}

	// 尝试从文件加载规则
	if err := manager.LoadRules(); err != nil {
		log.Printf("加载规则失败: %v", err)
	}

	// 尝试从文件加载统计信息
	if err := manager.LoadStats(); err != nil {
		log.Printf("加载统计信息失败: %v", err)
	}

	ruleManagers[key] = manager
	return manager
}

// LoadRules 从文件加载规则
func (rm *RuleManager) LoadRules() error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查文件是否存在
	if _, err := os.Stat(rm.rulesFile); os.IsNotExist(err) {
		// 文件不存在，创建默认规则
		rm.createDefaultRules()
		return rm.SaveRules()
	}

	// 读取文件内容
	data, err := ioutil.ReadFile(rm.rulesFile)
	if err != nil {
		return fmt.Errorf("读取规则文件失败: %v", err)
	}

	// 解析JSON
	if err := json.Unmarshal(data, rm); err != nil {
		return fmt.Errorf("解析规则文件失败: %v", err)
	}

	// 初始化必要的映射
	if rm.Rules == nil {
		rm.Rules = make(map[string]*LogRule)
	}

	if rm.Groups == nil {
		rm.Groups = make(map[string]*RuleGroup)
	}

	// 编译正则表达式
	for _, rule := range rm.Rules {
		if rule.MatchMode == MatchModeRegex {
			if regex, err := regexp.Compile(rule.Pattern); err == nil {
				rule.compiledRegex = regex
			} else {
				log.Printf("编译正则表达式失败: %v, 规则: %s", err, rule.Name)
			}
		}

		// 初始化子规则列表
		if rule.Type == RuleTypeGroup && rule.ChildRules == nil {
			rule.ChildRules = make([]string, 0)
		}
	}

	log.Printf("成功从文件加载 %d 条规则和 %d 个分组", len(rm.Rules), len(rm.Groups))
	return nil
}

// LoadStats 从文件加载统计信息
func (rm *RuleManager) LoadStats() error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查文件是否存在
	if _, err := os.Stat(rm.statsFile); os.IsNotExist(err) {
		// 文件不存在，初始化统计信息
		rm.Stats = RuleStats{
			TotalMatches: 0,
			RuleMatches:  make(map[string]int64),
			GroupMatches: make(map[string]int64),
			LastUpdated:  time.Now(),
		}
		return rm.SaveStats()
	}

	// 读取文件内容
	data, err := ioutil.ReadFile(rm.statsFile)
	if err != nil {
		return fmt.Errorf("读取统计文件失败: %v", err)
	}

	// 解析JSON
	var stats RuleStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return fmt.Errorf("解析统计文件失败: %v", err)
	}

	// 初始化必要的映射
	if stats.RuleMatches == nil {
		stats.RuleMatches = make(map[string]int64)
	}

	if stats.GroupMatches == nil {
		stats.GroupMatches = make(map[string]int64)
	}

	rm.Stats = stats
	log.Printf("成功从文件加载统计信息，总匹配次数: %d", stats.TotalMatches)
	return nil
}

// SaveRules 保存规则到文件
func (rm *RuleManager) SaveRules() error {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	// 序列化为JSON
	data, err := json.MarshalIndent(rm, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化规则失败: %v", err)
	}

	// 写入文件
	if err := ioutil.WriteFile(rm.rulesFile, data, 0644); err != nil {
		return fmt.Errorf("写入规则文件失败: %v", err)
	}

	log.Printf("成功保存 %d 条规则和 %d 个分组到文件", len(rm.Rules), len(rm.Groups))
	return nil
}

// SaveStats 保存统计信息到文件
func (rm *RuleManager) SaveStats() error {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	// 更新最后更新时间
	rm.Stats.LastUpdated = time.Now()

	// 序列化为JSON
	data, err := json.MarshalIndent(rm.Stats, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化统计信息失败: %v", err)
	}

	// 写入文件
	if err := ioutil.WriteFile(rm.statsFile, data, 0644); err != nil {
		return fmt.Errorf("写入统计文件失败: %v", err)
	}

	log.Printf("成功保存统计信息到文件，总匹配次数: %d", rm.Stats.TotalMatches)
	return nil
}

// createDefaultRules 创建默认规则
func (rm *RuleManager) createDefaultRules() {
	now := time.Now()

	// 创建默认分组
	systemGroup := &RuleGroup{
		ID:          "default_system",
		Name:        "系统信息",
		Description: "系统相关的日志信息",
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	playerGroup := &RuleGroup{
		ID:          "default_player",
		Name:        "玩家信息",
		Description: "玩家相关的日志信息",
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// 默认规则1：高亮错误信息
	errorRule := &LogRule{
		ID:          "default_error",
		Name:        "错误信息高亮",
		Type:        RuleTypeHighlight,
		MatchMode:   MatchModeContains,
		Pattern:     "ERROR",
		Color:       "#FF0000", // 红色
		Priority:    100,       // 高优先级
		Group:       systemGroup.ID,
		Enabled:     true,
		Description: "高亮显示包含ERROR的日志行",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// 默认规则2：高亮警告信息
	warningRule := &LogRule{
		ID:          "default_warning",
		Name:        "警告信息高亮",
		Type:        RuleTypeHighlight,
		MatchMode:   MatchModeContains,
		Pattern:     "WARNING",
		Color:       "#FFA500", // 橙色
		Priority:    90,        // 高优先级
		Group:       systemGroup.ID,
		Enabled:     true,
		Description: "高亮显示包含WARNING的日志行",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// 默认规则3：高亮玩家加入信息
	playerJoinRule := &LogRule{
		ID:          "default_player_join",
		Name:        "玩家加入高亮",
		Type:        RuleTypeHighlight,
		MatchMode:   MatchModeContains,
		Pattern:     "Player joined",
		Color:       "#00FF00", // 绿色
		Priority:    80,        // 高优先级
		Group:       playerGroup.ID,
		Enabled:     true,
		Description: "高亮显示玩家加入的日志行",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// 默认规则4：替换规则示例
	replaceRule := &LogRule{
		ID:          "default_replace",
		Name:        "替换日期格式",
		Type:        RuleTypeReplace,
		MatchMode:   MatchModeRegex,
		Pattern:     "\\[(\\d{4}-\\d{2}-\\d{2})\\]",
		Replacement: "[日期: $1]",
		Priority:    50, // 中等优先级
		Group:       systemGroup.ID,
		Enabled:     true,
		Description: "将日期格式替换为更易读的格式",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// 默认规则5：组合规则示例
	groupRule := &LogRule{
		ID:          "default_group",
		Name:        "重要系统信息",
		Type:        RuleTypeGroup,
		GroupMode:   GroupModeOr,
		ChildRules:  []string{errorRule.ID, warningRule.ID},
		Priority:    100, // 最高优先级
		Enabled:     true,
		Description: "匹配错误或警告信息",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// 添加默认分组
	rm.Groups[systemGroup.ID] = systemGroup
	rm.Groups[playerGroup.ID] = playerGroup

	// 添加默认规则
	rm.Rules[errorRule.ID] = errorRule
	rm.Rules[warningRule.ID] = warningRule
	rm.Rules[playerJoinRule.ID] = playerJoinRule
	rm.Rules[replaceRule.ID] = replaceRule
	rm.Rules[groupRule.ID] = groupRule
}

// AddRule 添加新规则
func (rm *RuleManager) AddRule(rule *LogRule) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 设置创建和更新时间
	now := time.Now()
	rule.CreatedAt = now
	rule.UpdatedAt = now

	// 编译正则表达式
	if rule.MatchMode == MatchModeRegex {
		regex, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return fmt.Errorf("正则表达式无效: %v", err)
		}
		rule.compiledRegex = regex
	}

	// 添加规则
	rm.Rules[rule.ID] = rule

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// UpdateRule 更新规则
func (rm *RuleManager) UpdateRule(rule *LogRule) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查规则是否存在
	if _, ok := rm.Rules[rule.ID]; !ok {
		return fmt.Errorf("规则不存在: %s", rule.ID)
	}

	// 保留创建时间
	rule.CreatedAt = rm.Rules[rule.ID].CreatedAt
	rule.UpdatedAt = time.Now()

	// 编译正则表达式
	if rule.MatchMode == MatchModeRegex {
		regex, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return fmt.Errorf("正则表达式无效: %v", err)
		}
		rule.compiledRegex = regex
	}

	// 更新规则
	rm.Rules[rule.ID] = rule

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// DeleteRule 删除规则
func (rm *RuleManager) DeleteRule(ruleID string) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查规则是否存在
	if _, ok := rm.Rules[ruleID]; !ok {
		return fmt.Errorf("规则不存在: %s", ruleID)
	}

	// 删除规则
	delete(rm.Rules, ruleID)

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// EnableRule 启用规则
func (rm *RuleManager) EnableRule(ruleID string) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查规则是否存在
	rule, ok := rm.Rules[ruleID]
	if !ok {
		return fmt.Errorf("规则不存在: %s", ruleID)
	}

	// 启用规则
	rule.Enabled = true
	rule.UpdatedAt = time.Now()

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// DisableRule 禁用规则
func (rm *RuleManager) DisableRule(ruleID string) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查规则是否存在
	rule, ok := rm.Rules[ruleID]
	if !ok {
		return fmt.Errorf("规则不存在: %s", ruleID)
	}

	// 禁用规则
	rule.Enabled = false
	rule.UpdatedAt = time.Now()

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// GetRules 获取所有规则
func (rm *RuleManager) GetRules() []*LogRule {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	rules := make([]*LogRule, 0, len(rm.Rules))
	for _, rule := range rm.Rules {
		rules = append(rules, rule)
	}

	return rules
}

// GetRule 获取指定规则
func (rm *RuleManager) GetRule(ruleID string) (*LogRule, error) {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	rule, ok := rm.Rules[ruleID]
	if !ok {
		return nil, fmt.Errorf("规则不存在: %s", ruleID)
	}

	return rule, nil
}

// GetGroups 获取所有规则分组
func (rm *RuleManager) GetGroups() []*RuleGroup {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	groups := make([]*RuleGroup, 0, len(rm.Groups))
	for _, group := range rm.Groups {
		groups = append(groups, group)
	}

	return groups
}

// GetGroup 获取指定规则分组
func (rm *RuleManager) GetGroup(groupID string) (*RuleGroup, error) {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	group, ok := rm.Groups[groupID]
	if !ok {
		return nil, fmt.Errorf("分组不存在: %s", groupID)
	}

	return group, nil
}

// AddGroup 添加新分组
func (rm *RuleManager) AddGroup(group *RuleGroup) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 设置创建和更新时间
	now := time.Now()
	group.CreatedAt = now
	group.UpdatedAt = now

	// 添加分组
	rm.Groups[group.ID] = group

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// UpdateGroup 更新分组
func (rm *RuleManager) UpdateGroup(group *RuleGroup) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查分组是否存在
	if _, ok := rm.Groups[group.ID]; !ok {
		return fmt.Errorf("分组不存在: %s", group.ID)
	}

	// 保留创建时间
	group.CreatedAt = rm.Groups[group.ID].CreatedAt
	group.UpdatedAt = time.Now()

	// 更新分组
	rm.Groups[group.ID] = group

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// DeleteGroup 删除分组
func (rm *RuleManager) DeleteGroup(groupID string) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查分组是否存在
	if _, ok := rm.Groups[groupID]; !ok {
		return fmt.Errorf("分组不存在: %s", groupID)
	}

	// 删除分组
	delete(rm.Groups, groupID)

	// 更新引用该分组的规则
	for _, rule := range rm.Rules {
		if rule.Group == groupID {
			rule.Group = ""
		}
	}

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// EnableGroup 启用分组
func (rm *RuleManager) EnableGroup(groupID string) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查分组是否存在
	group, ok := rm.Groups[groupID]
	if !ok {
		return fmt.Errorf("分组不存在: %s", groupID)
	}

	// 启用分组
	group.Enabled = true
	group.UpdatedAt = time.Now()

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// DisableGroup 禁用分组
func (rm *RuleManager) DisableGroup(groupID string) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 检查分组是否存在
	group, ok := rm.Groups[groupID]
	if !ok {
		return fmt.Errorf("分组不存在: %s", groupID)
	}

	// 禁用分组
	group.Enabled = false
	group.UpdatedAt = time.Now()

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// GetRulesByGroup 获取指定分组的所有规则
func (rm *RuleManager) GetRulesByGroup(groupID string) []*LogRule {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	rules := make([]*LogRule, 0)
	for _, rule := range rm.Rules {
		if rule.Group == groupID {
			rules = append(rules, rule)
		}
	}

	return rules
}

// ExportRuleData 导出规则数据
type ExportRuleData struct {
	Rules       []*LogRule   `json:"rules"`       // 规则列表
	Groups      []*RuleGroup `json:"groups"`      // 分组列表
	Version     string       `json:"version"`     // 版本号
	ExportTime  time.Time    `json:"export_time"` // 导出时间
	Description string       `json:"description"` // 描述
}

// ExportRules 导出规则
func (rm *RuleManager) ExportRules(description string) (*ExportRuleData, error) {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	// 收集规则和分组
	rules := rm.GetRules()
	groups := rm.GetGroups()

	// 创建导出数据
	exportData := &ExportRuleData{
		Rules:       rules,
		Groups:      groups,
		Version:     "1.0",
		ExportTime:  time.Now(),
		Description: description,
	}

	return exportData, nil
}

// ImportRules 导入规则
func (rm *RuleManager) ImportRules(data *ExportRuleData, overwrite bool) error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 如果选择覆盖，则清空现有规则和分组
	if overwrite {
		rm.Rules = make(map[string]*LogRule)
		rm.Groups = make(map[string]*RuleGroup)
	}

	// 导入分组
	for _, group := range data.Groups {
		// 如果分组已存在且不覆盖，则跳过
		if !overwrite && rm.Groups[group.ID] != nil {
			continue
		}

		// 添加分组
		rm.Groups[group.ID] = group
	}

	// 导入规则
	for _, rule := range data.Rules {
		// 如果规则已存在且不覆盖，则跳过
		if !overwrite && rm.Rules[rule.ID] != nil {
			continue
		}

		// 编译正则表达式
		if rule.MatchMode == MatchModeRegex {
			if regex, err := regexp.Compile(rule.Pattern); err == nil {
				rule.compiledRegex = regex
			} else {
				log.Printf("编译正则表达式失败: %v, 规则: %s", err, rule.Name)
			}
		}

		// 添加规则
		rm.Rules[rule.ID] = rule
	}

	// 保存到文件
	go rm.SaveRules()

	return nil
}

// GetStats 获取统计信息
func (rm *RuleManager) GetStats() RuleStats {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	return rm.Stats
}

// ResetStats 重置统计信息
func (rm *RuleManager) ResetStats() error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	// 重置统计信息
	rm.Stats = RuleStats{
		TotalMatches: 0,
		RuleMatches:  make(map[string]int64),
		GroupMatches: make(map[string]int64),
		LastUpdated:  time.Now(),
	}

	// 重置规则匹配次数
	for _, rule := range rm.Rules {
		rule.MatchCount = 0
	}

	// 保存到文件
	return rm.SaveStats()
}

// TestRuleResult 测试规则结果
type TestRuleResult struct {
	Rule        *LogRule `json:"rule"`        // 规则
	Matched     bool     `json:"matched"`     // 是否匹配
	ResultLine  string   `json:"result_line"` // 处理后的行
	Highlighted bool     `json:"highlighted"` // 是否高亮
	Color       string   `json:"color"`       // 高亮颜色
}

// TestRule 测试规则
func (rm *RuleManager) TestRule(ruleID string, line string) (*TestRuleResult, error) {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	// 获取规则
	rule, ok := rm.Rules[ruleID]
	if !ok {
		return nil, fmt.Errorf("规则不存在: %s", ruleID)
	}

	// 初始化结果
	result := &TestRuleResult{
		Rule:        rule,
		Matched:     false,
		ResultLine:  line,
		Highlighted: false,
		Color:       "",
	}

	// 根据规则类型测试
	switch rule.Type {
	case RuleTypeInclude, RuleTypeExclude:
		// 测试是否匹配
		result.Matched = rm.matchRule(rule, line)

	case RuleTypeHighlight:
		// 测试是否匹配
		result.Matched = rm.matchRule(rule, line)
		if result.Matched {
			result.Highlighted = true
			result.Color = rule.Color
		}

	case RuleTypeReplace:
		// 测试是否匹配
		result.Matched = rm.matchRule(rule, line)
		if result.Matched {
			// 应用替换
			result.ResultLine = rm.applyReplacement(rule, line)
		}

	case RuleTypeGroup:
		// 测试组合规则
		result.Matched = rm.matchGroupRule(rule, line)
	}

	return result, nil
}

// TestAllRules 测试所有规则
func (rm *RuleManager) TestAllRules(line string) ([]*TestRuleResult, error) {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	// 按优先级排序规则
	rules := rm.getSortedRules()

	// 初始化结果列表
	results := make([]*TestRuleResult, 0, len(rules))

	// 测试每个规则
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}

		// 测试规则
		result, _ := rm.TestRule(rule.ID, line)
		if result != nil {
			results = append(results, result)
		}
	}

	return results, nil
}

// TestLine 测试日志行
func (rm *RuleManager) TestLine(line string) (string, bool, string) {
	// 使用实际的过滤逻辑测试行，但不更新统计信息
	// 这里我们复制 FilterLogLine 的逻辑，但不调用 updateRuleStats 和 updateGroupStats

	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	// 按优先级排序规则
	rules := rm.getSortedRules()

	// 处理结果
	resultLine := line
	showLine := true
	color := ""

	// 首先检查组合规则
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeGroup {
			continue
		}

		// 判断组合规则是否匹配
		rm.matchGroupRule(rule, line)
	}

	// 检查排除规则
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeExclude {
			continue
		}

		if rm.matchRule(rule, resultLine) {
			// 匹配到排除规则，不显示该行
			return "", false, ""
		}
	}

	// 检查包含规则
	hasIncludeRule := false
	includeMatch := false

	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeInclude {
			continue
		}

		hasIncludeRule = true
		if rm.matchRule(rule, resultLine) {
			includeMatch = true
			break
		}
	}

	// 如果有包含规则但没有匹配，则不显示该行
	if hasIncludeRule && !includeMatch {
		return "", false, ""
	}

	// 应用替换规则
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeReplace {
			continue
		}

		if rm.matchRule(rule, resultLine) {
			// 应用替换
			resultLine = rm.applyReplacement(rule, resultLine)
		}
	}

	// 检查高亮规则
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeHighlight {
			continue
		}

		if rm.matchRule(rule, resultLine) {
			// 匹配到高亮规则，返回高亮颜色
			color = rule.Color
			break
		}
	}

	// 返回处理后的结果
	return resultLine, showLine, color
}

// FilterLogLine 根据规则过滤日志行
func (rm *RuleManager) FilterLogLine(line string) (string, bool, string) {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	// 按优先级排序规则
	rules := rm.getSortedRules()

	// 处理结果
	resultLine := line
	showLine := true
	color := ""

	// 首先检查组合规则
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeGroup {
			continue
		}

		// 判断组合规则是否匹配
		if rm.matchGroupRule(rule, line) {
			// 更新统计信息
			rm.updateRuleStats(rule.ID)

			// 如果有组匹配，则更新组统计
			if rule.Group != "" {
				rm.updateGroupStats(rule.Group)
			}
		}
	}

	// 检查排除规则
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeExclude {
			continue
		}

		if rm.matchRule(rule, resultLine) {
			// 更新统计信息
			rm.updateRuleStats(rule.ID)

			// 匹配到排除规则，不显示该行
			return "", false, ""
		}
	}

	// 检查包含规则（如果有启用的包含规则，则只显示匹配的行）
	hasIncludeRule := false
	includeMatch := false

	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeInclude {
			continue
		}

		hasIncludeRule = true
		if rm.matchRule(rule, resultLine) {
			// 更新统计信息
			rm.updateRuleStats(rule.ID)

			includeMatch = true
			break
		}
	}

	// 如果有包含规则但没有匹配，则不显示该行
	if hasIncludeRule && !includeMatch {
		return "", false, ""
	}

	// 应用替换规则
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeReplace {
			continue
		}

		if rm.matchRule(rule, resultLine) {
			// 更新统计信息
			rm.updateRuleStats(rule.ID)

			// 应用替换
			resultLine = rm.applyReplacement(rule, resultLine)
		}
	}

	// 检查高亮规则
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != RuleTypeHighlight {
			continue
		}

		if rm.matchRule(rule, resultLine) {
			// 更新统计信息
			rm.updateRuleStats(rule.ID)

			// 匹配到高亮规则，返回高亮颜色
			color = rule.Color
			break
		}
	}

	// 返回处理后的结果
	return resultLine, showLine, color
}

// getSortedRules 按优先级排序规则
func (rm *RuleManager) getSortedRules() []*LogRule {
	rules := make([]*LogRule, 0, len(rm.Rules))

	// 收集所有规则
	for _, rule := range rm.Rules {
		rules = append(rules, rule)
	}

	// 按优先级排序（优先级高的先执行）
	sort.Slice(rules, func(i, j int) bool {
		return rules[i].Priority > rules[j].Priority
	})

	return rules
}

// matchGroupRule 检查日志行是否匹配组合规则
func (rm *RuleManager) matchGroupRule(rule *LogRule, line string) bool {
	// 如果不是组合规则或没有子规则，返回否
	if rule.Type != RuleTypeGroup || len(rule.ChildRules) == 0 {
		return false
	}

	// 根据组合模式判断
	switch rule.GroupMode {
	case GroupModeAnd: // 所有子规则都匹配
		for _, childID := range rule.ChildRules {
			childRule, ok := rm.Rules[childID]
			if !ok || !childRule.Enabled {
				return false
			}

			if !rm.matchRule(childRule, line) {
				return false
			}

			// 更新子规则的统计信息
			rm.updateRuleStats(childID)
		}
		return true

	case GroupModeOr: // 任一子规则匹配
		for _, childID := range rule.ChildRules {
			childRule, ok := rm.Rules[childID]
			if !ok || !childRule.Enabled {
				continue
			}

			if rm.matchRule(childRule, line) {
				// 更新子规则的统计信息
				rm.updateRuleStats(childID)
				return true
			}
		}
		return false

	default:
		return false
	}
}

// applyReplacement 应用替换规则
func (rm *RuleManager) applyReplacement(rule *LogRule, line string) string {
	if rule.Type != RuleTypeReplace {
		return line
	}

	switch rule.MatchMode {
	case MatchModeRegex:
		if rule.compiledRegex != nil {
			return rule.compiledRegex.ReplaceAllString(line, rule.Replacement)
		}
		// 尝试临时编译正则表达式
		regex, err := regexp.Compile(rule.Pattern)
		if err != nil {
			log.Printf("编译正则表达式失败: %v, 规则: %s", err, rule.Name)
			return line
		}
		rule.compiledRegex = regex
		return regex.ReplaceAllString(line, rule.Replacement)

	case MatchModeContains:
		return strings.Replace(line, rule.Pattern, rule.Replacement, -1)

	case MatchModePrefix:
		if strings.HasPrefix(line, rule.Pattern) {
			return rule.Replacement + line[len(rule.Pattern):]
		}
		return line

	case MatchModeSuffix:
		if strings.HasSuffix(line, rule.Pattern) {
			return line[:len(line)-len(rule.Pattern)] + rule.Replacement
		}
		return line

	default:
		return line
	}
}

// updateRuleStats 更新规则统计信息
func (rm *RuleManager) updateRuleStats(ruleID string) {
	// 更新总匹配次数
	rm.Stats.TotalMatches++

	// 更新规则匹配次数
	rm.Stats.RuleMatches[ruleID]++

	// 更新规则对象的匹配次数
	if rule, ok := rm.Rules[ruleID]; ok {
		rule.MatchCount++
	}

	// 定期保存统计信息（每100次匹配保存一次）
	if rm.Stats.TotalMatches%100 == 0 {
		go rm.SaveStats()
	}
}

// updateGroupStats 更新分组统计信息
func (rm *RuleManager) updateGroupStats(groupID string) {
	// 更新分组匹配次数
	rm.Stats.GroupMatches[groupID]++
}

// matchRule 检查日志行是否匹配规则
func (rm *RuleManager) matchRule(rule *LogRule, line string) bool {
	switch rule.MatchMode {
	case MatchModeContains:
		return strings.Contains(line, rule.Pattern)

	case MatchModeNotContains:
		return !strings.Contains(line, rule.Pattern)

	case MatchModeRegex:
		if rule.compiledRegex != nil {
			return rule.compiledRegex.MatchString(line)
		}
		// 尝试临时编译正则表达式
		regex, err := regexp.Compile(rule.Pattern)
		if err != nil {
			log.Printf("编译正则表达式失败: %v, 规则: %s", err, rule.Name)
			return false
		}
		rule.compiledRegex = regex
		return regex.MatchString(line)

	case MatchModePrefix:
		return strings.HasPrefix(line, rule.Pattern)

	case MatchModeSuffix:
		return strings.HasSuffix(line, rule.Pattern)

	case MatchModeEquals:
		return line == rule.Pattern

	case MatchModeNotEquals:
		return line != rule.Pattern

	default:
		return false
	}
}
