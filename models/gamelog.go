package models

import (
	"fmt"
	"log"
	"time"
)

// 使用标准日志包作为日志记录器
var logger = log.New(log.Writer(), "[GameLog] ", log.LstdFlags)

// GameLog 游戏日志记录
type GameLog struct {
	ID          int       `gorm:"primary_key" json:"id"`
	ArchiveName string    `json:"archive_name"` // 存档名称
	WorldName   string    `json:"world_name"`   // 世界名称
	LogType     string    `json:"log_type"`     // 日志类型
	Content     string    `json:"content"`      // 日志内容
	RawContent  string    `json:"raw_content"`  // 原始日志内容
	Timestamp   time.Time `json:"timestamp"`    // 日志时间戳
	CreatedAt   time.Time `json:"created_at"`   // 记录创建时间
}

// LogType 日志类型常量
const (
	LogTypeSystem  = "system"  // 系统日志
	LogTypeChat    = "chat"    // 聊天日志
	LogTypePlayer  = "player"  // 玩家行为日志
	LogTypeEntity  = "entity"  // 实体相关日志
	LogTypeWorld   = "world"   // 世界事件日志
	LogTypeError   = "error"   // 错误日志
	LogTypeWarning = "warning" // 警告日志
	LogTypeUnknown = "unknown" // 未知类型
)

// LogExtractRule 日志提取规则
type LogExtractRule struct {
	ID          int       `gorm:"primary_key" json:"id"`
	Name        string    `json:"name"`        // 规则名称
	Description string    `json:"description"` // 规则描述
	LogType     string    `json:"log_type"`    // 提取的日志类型
	Pattern     string    `json:"pattern"`     // 匹配模式（正则表达式）
	IsRegex     bool      `json:"is_regex"`    // 是否使用正则表达式
	IsEnabled   bool      `json:"is_enabled"`  // 是否启用
	Priority    int       `json:"priority"`    // 优先级（数字越大优先级越高）
	CreatedAt   time.Time `json:"created_at"`  // 创建时间
	UpdatedAt   time.Time `json:"updated_at"`  // 更新时间
}

// LogStatistics 日志统计信息
type LogStatistics struct {
	ID          int       `gorm:"primary_key" json:"id"`
	ArchiveName string    `json:"archive_name"` // 存档名称
	WorldName   string    `json:"world_name"`   // 世界名称
	Date        time.Time `json:"date"`         // 统计日期
	LogType     string    `json:"log_type"`     // 日志类型
	Count       int       `json:"count"`        // 日志数量
}

// 初始化表结构
func InitGameLogTables() {
	// 自动迁移表结构
	db.AutoMigrate(&GameLog{})
	db.AutoMigrate(&LogExtractRule{})
	db.AutoMigrate(&LogStatistics{})
}

// AddGameLog 添加游戏日志记录
func AddGameLog(archiveName, worldName, logType, content, rawContent string, timestamp time.Time) error {
	// 打印调试信息
	logger.Printf("[Models] AddGameLog: 存档=%s, 世界=%s, 类型=%s, 内容=%s",
		archiveName, worldName, logType, content)

	// 检查数据库连接
	if db == nil {
		logger.Printf("[Models] AddGameLog 失败: 数据库连接为空")
		return fmt.Errorf("数据库连接为空")
	}

	log := GameLog{
		ArchiveName: archiveName,
		WorldName:   worldName,
		LogType:     logType,
		Content:     content,
		RawContent:  rawContent,
		Timestamp:   timestamp,
		CreatedAt:   time.Now(),
	}

	// 打印详细的SQL日志
	db.LogMode(true)

	if err := db.Create(&log).Error; err != nil {
		logger.Printf("[Models] AddGameLog 失败: %v", err)
		return err
	}

	logger.Printf("[Models] AddGameLog 成功: ID=%d", log.ID)

	// 更新统计信息
	updateLogStatistics(archiveName, worldName, logType, timestamp)

	return nil
}

// GetGameLogs 获取游戏日志记录
func GetGameLogs(archiveName, worldName, logType string, startTime, endTime time.Time, page, pageSize int) ([]GameLog, int, error) {
	logger.Printf("[Models] GetGameLogs: 开始查询日志记录, 存档=%s, 世界=%s, 类型=%s, 页码=%d, 每页数量=%d",
		archiveName, worldName, logType, page, pageSize)

	var logs []GameLog
	var count int

	// 检查数据库连接
	if db == nil {
		logger.Printf("[Models] GetGameLogs 失败: 数据库连接为空")
		return nil, 0, fmt.Errorf("数据库连接为空")
	}

	query := db.Model(&GameLog{})

	// 添加查询条件
	if archiveName != "" {
		query = query.Where("archive_name = ?", archiveName)
	}
	if worldName != "" {
		query = query.Where("world_name = ?", worldName)
	}
	if logType != "" {
		query = query.Where("log_type = ?", logType)
	}
	if !startTime.IsZero() {
		query = query.Where("timestamp >= ?", startTime)
	}
	if !endTime.IsZero() {
		query = query.Where("timestamp <= ?", endTime)
	}

	logger.Printf("[Models] GetGameLogs: 构建查询条件完成, 开始查询总数")

	// 获取总数
	if err := query.Count(&count).Error; err != nil {
		logger.Printf("[Models] GetGameLogs 查询总数失败: %v", err)
		return nil, 0, err
	}

	logger.Printf("[Models] GetGameLogs: 查询到总数: %d", count)

	// 分页查询
	offset := (page - 1) * pageSize
	logger.Printf("[Models] GetGameLogs: 开始分页查询, offset=%d, limit=%d", offset, pageSize)

	if err := query.Order("timestamp desc").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		logger.Printf("[Models] GetGameLogs 分页查询失败: %v", err)
		return nil, 0, err
	}

	logger.Printf("[Models] GetGameLogs: 查询成功, 返回 %d 条记录", len(logs))
	return logs, count, nil
}

// 更新日志统计信息
func updateLogStatistics(archiveName, worldName, logType string, timestamp time.Time) error {
	// 获取日期（不包含时间）
	date := time.Date(timestamp.Year(), timestamp.Month(), timestamp.Day(), 0, 0, 0, 0, timestamp.Location())

	// 查找现有统计记录
	var stat LogStatistics
	result := db.Where("archive_name = ? AND world_name = ? AND date = ? AND log_type = ?",
		archiveName, worldName, date, logType).First(&stat)

	if result.Error != nil {
		// 如果记录不存在，创建新记录
		if result.RecordNotFound() {
			stat = LogStatistics{
				ArchiveName: archiveName,
				WorldName:   worldName,
				Date:        date,
				LogType:     logType,
				Count:       1,
			}
			return db.Create(&stat).Error
		}
		return result.Error
	}

	// 更新计数
	stat.Count++
	return db.Save(&stat).Error
}

// AddLogExtractRule 添加日志提取规则
func AddLogExtractRule(name, description, logType, pattern string, isRegex, isEnabled bool, priority int) error {
	rule := LogExtractRule{
		Name:        name,
		Description: description,
		LogType:     logType,
		Pattern:     pattern,
		IsRegex:     isRegex,
		IsEnabled:   isEnabled,
		Priority:    priority,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	return db.Create(&rule).Error
}

// UpdateLogExtractRule 更新日志提取规则
func UpdateLogExtractRule(id int, name, description, logType, pattern string, isRegex, isEnabled bool, priority int) error {
	updates := map[string]interface{}{
		"name":        name,
		"description": description,
		"log_type":    logType,
		"pattern":     pattern,
		"is_regex":    isRegex,
		"is_enabled":  isEnabled,
		"priority":    priority,
		"updated_at":  time.Now(),
	}

	return db.Model(&LogExtractRule{}).Where("id = ?", id).Updates(updates).Error
}

// DeleteLogExtractRule 删除日志提取规则
func DeleteLogExtractRule(id int) error {
	return db.Delete(&LogExtractRule{}, id).Error
}

// GetLogExtractRules 获取所有日志提取规则
func GetLogExtractRules() ([]LogExtractRule, error) {
	var rules []LogExtractRule
	err := db.Order("priority desc").Find(&rules).Error
	return rules, err
}

// GetEnabledLogExtractRules 获取所有启用的日志提取规则
func GetEnabledLogExtractRules() ([]LogExtractRule, error) {
	var rules []LogExtractRule
	err := db.Where("is_enabled = ?", true).Order("priority desc").Find(&rules).Error
	return rules, err
}

// GetLogStatistics 获取日志统计信息
func GetLogStatistics(archiveName, worldName string, startDate, endDate time.Time) ([]LogStatistics, error) {
	var stats []LogStatistics
	query := db.Model(&LogStatistics{})

	if archiveName != "" {
		query = query.Where("archive_name = ?", archiveName)
	}
	if worldName != "" {
		query = query.Where("world_name = ?", worldName)
	}
	if !startDate.IsZero() {
		query = query.Where("date >= ?", startDate)
	}
	if !endDate.IsZero() {
		query = query.Where("date <= ?", endDate)
	}

	err := query.Order("date desc").Find(&stats).Error
	return stats, err
}

// ClearOldLogs 清理指定日期之前的日志
func ClearOldLogs(beforeDate time.Time) error {
	return db.Where("timestamp < ?", beforeDate).Delete(&GameLog{}).Error
}

// GetLogTypeCount 获取各类型日志的数量
func GetLogTypeCount(archiveName, worldName string) (map[string]int, error) {
	type Result struct {
		LogType string
		Count   int
	}

	var results []Result
	query := db.Model(&GameLog{}).Select("log_type, count(*) as count").Group("log_type")

	if archiveName != "" {
		query = query.Where("archive_name = ?", archiveName)
	}
	if worldName != "" {
		query = query.Where("world_name = ?", worldName)
	}

	if err := query.Scan(&results).Error; err != nil {
		return nil, err
	}

	// 转换为map
	countMap := make(map[string]int)
	for _, r := range results {
		countMap[r.LogType] = r.Count
	}

	return countMap, nil
}

// SearchGameLogs 搜索游戏日志
func SearchGameLogs(keyword string, page, pageSize int) ([]GameLog, int, error) {
	var logs []GameLog
	var count int

	query := db.Model(&GameLog{}).Where("content LIKE ? OR raw_content LIKE ?",
		"%"+keyword+"%", "%"+keyword+"%")

	// 获取总数
	if err := query.Count(&count).Error; err != nil {
		return nil, 0, err
	}

	// 分页查询
	offset := (page - 1) * pageSize
	if err := query.Order("timestamp desc").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}

	return logs, count, nil
}

// GetGameLogCount 获取日志总数
func GetGameLogCount() (int, error) {
	var count int
	if err := db.Model(&GameLog{}).Count(&count).Error; err != nil {
		logger.Printf("[Models] GetGameLogCount 失败: %v", err)
		return 0, err
	}
	return count, nil
}
