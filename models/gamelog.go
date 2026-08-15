package models

import (
	"bufio"
	"dont/pkg/configpath"
	"fmt"
	"github.com/go-ini/ini"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 使用标准日志包作为日志记录器
var logger = log.New(log.Writer(), "[GameLog] ", log.LstdFlags)

// 统计缓存相关
var (
	statCache      = make(map[string]int) // 统计缓存
	statCacheMutex sync.RWMutex           // 缓存读写锁
	lastFlushTime  = time.Now()           // 上次刷新缓存的时间
	cacheThreshold = 50                   // 缓存阈值，超过此值将触发批量写入
	flushInterval  = 30 * time.Second     // 定时刷新间隔
)

// GameLog 游戏日志记录
type GameLog struct {
	ID             int       `gorm:"primary_key" json:"id"`
	ArchiveName    string    `json:"archive_name"`    // 存档名称
	WorldName      string    `json:"world_name"`      // 世界名称
	LogType        string    `json:"log_type"`        // 日志类型
	Content        string    `json:"content"`         // 日志内容
	RawContent     string    `json:"raw_content"`     // 原始日志内容
	Timestamp      time.Time `json:"timestamp"`       // 日志时间戳
	CreatedAt      time.Time `json:"created_at"`      // 记录创建时间
	StartupVersion string    `json:"startup_version"` // 服务启动版本
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

// 日志匹配模式常量
const (
	MatchModeSingle     = "single"      // 单行匹配模式（默认）
	MatchModeMultiLine  = "multi_line"  // 多行匹配模式
	MatchModeHeadTail   = "head_tail"   // 首尾行匹配模式
	MatchModeFixedLines = "fixed_lines" // 固定行数匹配模式
)

// LogExtractRule 日志提取规则
type LogExtractRule struct {
	ID          int       `gorm:"primary_key" json:"id"`
	Name        string    `json:"name"`         // 规则名称
	Description string    `json:"description"`  // 规则描述
	LogType     string    `json:"log_type"`     // 提取的日志类型
	Pattern     string    `json:"pattern"`      // 匹配模式（正则表达式）
	IsRegex     bool      `json:"is_regex"`     // 是否使用正则表达式
	IsEnabled   bool      `json:"is_enabled"`   // 是否启用
	Priority    int       `json:"priority"`     // 优先级（数字越大优先级越高）
	MatchMode   string    `json:"match_mode"`   // 匹配模式：single(单行), multi_line(多行), head_tail(首尾行), fixed_lines(固定行数)
	TailPattern string    `json:"tail_pattern"` // 尾行匹配模式（仅当match_mode为head_tail时有效）
	LineCount   int       `json:"line_count"`   // 固定行数（仅当match_mode为fixed_lines时有效）
	CreatedAt   time.Time `json:"created_at"`   // 创建时间
	UpdatedAt   time.Time `json:"updated_at"`   // 更新时间
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

	// 添加索引以提高查询性能
	// 为 timestamp 字段添加索引
	db.Model(&GameLog{}).AddIndex("idx_game_log_timestamp", "timestamp")
	// 为 archive_name, world_name, timestamp 添加组合索引
	db.Model(&GameLog{}).AddIndex("idx_game_log_archive_world_timestamp", "archive_name", "world_name", "timestamp")
	// 为 startup_version 添加索引
	db.Model(&GameLog{}).AddIndex("idx_game_log_startup_version", "startup_version")

	// 启动定时刷新统计缓存的协程
	go startStatCacheFlushTimer()

	logger.Printf("[Models] 游戏日志表初始化完成，已添加索引")
}

// 启动定时刷新统计缓存的定时器
func startStatCacheFlushTimer() {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for range ticker.C {
		FlushStatCache()
	}
}

// FlushStatCache 刷新统计缓存到数据库
func FlushStatCache() {
	statCacheMutex.Lock()
	defer statCacheMutex.Unlock()

	// 如果缓存为空，直接返回
	if len(statCache) == 0 {
		return
	}

	logger.Printf("[Models] 开始刷新统计缓存到数据库，缓存条目数: %d", len(statCache))

	// 使用事务批量更新
	tx := db.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if err := tx.Error; err != nil {
		logger.Printf("[Models] FlushStatCache 开始事务失败: %v", err)
		return
	}

	// 复制缓存内容并清空缓存
	tempCache := make(map[string]int)
	for k, v := range statCache {
		tempCache[k] = v
	}
	statCache = make(map[string]int)

	// 更新最后刷新时间
	lastFlushTime = time.Now()

	// 批量更新统计信息
	for key, count := range tempCache {
		// 解析键
		parts := strings.Split(key, ":")
		if len(parts) != 4 {
			continue
		}
		archiveName := parts[0]
		worldName := parts[1]
		logType := parts[2]
		dateStr := parts[3]
		date, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}

		// 查找现有统计记录
		var stat LogStatistics
		result := tx.Where("archive_name = ? AND world_name = ? AND date = ? AND log_type = ?",
			archiveName, worldName, date, logType).First(&stat)

		if result.Error != nil {
			// 如果记录不存在，创建新记录
			if result.RecordNotFound() {
				stat = LogStatistics{
					ArchiveName: archiveName,
					WorldName:   worldName,
					Date:        date,
					LogType:     logType,
					Count:       count,
				}
				if err := tx.Create(&stat).Error; err != nil {
					tx.Rollback()
					logger.Printf("[Models] FlushStatCache 创建统计记录失败: %v", err)
					return
				}
			} else {
				tx.Rollback()
				logger.Printf("[Models] FlushStatCache 查询统计记录失败: %v", result.Error)
				return
			}
		} else {
			// 更新计数
			stat.Count += count
			if err := tx.Save(&stat).Error; err != nil {
				tx.Rollback()
				logger.Printf("[Models] FlushStatCache 更新统计记录失败: %v", err)
				return
			}
		}
	}

	// 提交事务
	if err := tx.Commit().Error; err != nil {
		logger.Printf("[Models] FlushStatCache 提交事务失败: %v", err)
		return
	}

	logger.Printf("[Models] 成功刷新统计缓存到数据库，共 %d 条记录", len(tempCache))
}

// AddGameLog 添加游戏日志记录
// updateStats 参数控制是否更新统计信息，在批量处理时可以设为false以提高性能
func AddGameLog(archiveName, worldName, logType, content, rawContent string, timestamp time.Time, startupVersion string, updateStats bool) error {
	// 打印调试信息
	logger.Printf("[Models] AddGameLog: 存档=%s, 世界=%s, 类型=%s, 内容=%s, 启动版本=%s",
		archiveName, worldName, logType, content, startupVersion)

	// 检查数据库连接
	if db == nil {
		logger.Printf("[Models] AddGameLog 失败: 数据库连接为空")
		return fmt.Errorf("数据库连接为空")
	}

	log := GameLog{
		ArchiveName:    archiveName,
		WorldName:      worldName,
		LogType:        logType,
		Content:        content,
		RawContent:     rawContent,
		Timestamp:      timestamp,
		StartupVersion: startupVersion,
		CreatedAt:      time.Now(),
	}

	// 打印详细的SQL日志
	db.LogMode(true)

	if err := db.Create(&log).Error; err != nil {
		logger.Printf("[Models] AddGameLog 失败: %v", err)
		return err
	}

	logger.Printf("[Models] AddGameLog 成功: ID=%d", log.ID)

	// 根据参数决定是否更新统计信息
	if updateStats {
		updateLogStatistics(archiveName, worldName, logType, timestamp)
	}

	return nil
}

// AddGameLogBatch 批量添加游戏日志记录
func AddGameLogBatch(logs []GameLog) error {
	// 检查数据库连接
	if db == nil {
		logger.Printf("[Models] AddGameLogBatch 失败: 数据库连接为空")
		return fmt.Errorf("数据库连接为空")
	}

	// 如果没有日志，直接返回
	if len(logs) == 0 {
		return nil
	}

	// 打印调试信息
	logger.Printf("[Models] AddGameLogBatch: 批量添加 %d 条日志记录", len(logs))

	// 使用事务批量插入
	tx := db.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if err := tx.Error; err != nil {
		logger.Printf("[Models] AddGameLogBatch 开始事务失败: %v", err)
		return err
	}

	// 批量插入日志
	for _, log := range logs {
		if err := tx.Create(&log).Error; err != nil {
			tx.Rollback()
			logger.Printf("[Models] AddGameLogBatch 插入日志失败: %v", err)
			return err
		}
	}

	// 收集统计信息
	statMap := make(map[string]int)
	for _, log := range logs {
		// 获取日期（不包含时间）
		date := time.Date(log.Timestamp.Year(), log.Timestamp.Month(), log.Timestamp.Day(), 0, 0, 0, 0, log.Timestamp.Location())
		// 创建统计键
		key := fmt.Sprintf("%s:%s:%s:%s", log.ArchiveName, log.WorldName, log.LogType, date.Format("2006-01-02"))
		// 增加计数
		statMap[key]++
	}

	// 批量更新统计信息
	for key, count := range statMap {
		// 解析键
		parts := strings.Split(key, ":")
		if len(parts) != 4 {
			continue
		}
		archiveName := parts[0]
		worldName := parts[1]
		logType := parts[2]
		dateStr := parts[3]
		date, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}

		// 查找现有统计记录
		var stat LogStatistics
		result := tx.Where("archive_name = ? AND world_name = ? AND date = ? AND log_type = ?",
			archiveName, worldName, date, logType).First(&stat)

		if result.Error != nil {
			// 如果记录不存在，创建新记录
			if result.RecordNotFound() {
				stat = LogStatistics{
					ArchiveName: archiveName,
					WorldName:   worldName,
					Date:        date,
					LogType:     logType,
					Count:       count,
				}
				if err := tx.Create(&stat).Error; err != nil {
					tx.Rollback()
					logger.Printf("[Models] AddGameLogBatch 创建统计记录失败: %v", err)
					return err
				}
			} else {
				tx.Rollback()
				logger.Printf("[Models] AddGameLogBatch 查询统计记录失败: %v", result.Error)
				return result.Error
			}
		} else {
			// 更新计数
			stat.Count += count
			if err := tx.Save(&stat).Error; err != nil {
				tx.Rollback()
				logger.Printf("[Models] AddGameLogBatch 更新统计记录失败: %v", err)
				return err
			}
		}
	}

	// 提交事务
	if err := tx.Commit().Error; err != nil {
		logger.Printf("[Models] AddGameLogBatch 提交事务失败: %v", err)
		return err
	}

	logger.Printf("[Models] AddGameLogBatch 成功: 添加了 %d 条日志记录", len(logs))
	return nil
}

// GetGameLogs 获取游戏日志记录
func GetGameLogs(archiveName, worldName, logType, startupVersion string, startTime, endTime time.Time, page, pageSize int) ([]GameLog, int, error) {
	logger.Printf("[Models] GetGameLogs: 开始查询日志记录, 存档=%s, 世界=%s, 类型=%s, 启动版本=%s, 页码=%d, 每页数量=%d",
		archiveName, worldName, logType, startupVersion, page, pageSize)

	var logs []GameLog
	var count int

	// 检查数据库连接
	if db == nil {
		logger.Printf("[Models] GetGameLogs 失败: 数据库连接为空")
		return nil, 0, fmt.Errorf("数据库连接为空")
	}

	// 如果没有指定启动版本，获取最新的启动版本
	if startupVersion == "" && archiveName != "" && worldName != "" {
		versions, err := GetStartupVersions(archiveName, worldName)
		if err == nil && len(versions) > 0 {
			// 使用最新的启动版本
			startupVersion = versions[0]
			logger.Printf("[Models] GetGameLogs: 自动选择最新的启动版本: %s", startupVersion)
		}
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
	if startupVersion != "" {
		query = query.Where("startup_version = ?", startupVersion)
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

	// 按时间戳和创建时间排序，确保日志顺序正确
	if err := query.Order("timestamp desc, created_at desc").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		logger.Printf("[Models] GetGameLogs 分页查询失败: %v", err)
		return nil, 0, err
	}

	// 打印日志时间戳信息以便调试
	if len(logs) > 0 {
		logger.Printf("[Models] GetGameLogs: 日志时间戳范围: 最早=%s, 最晚=%s",
			logs[len(logs)-1].Timestamp.Format("2006-01-02 15:04:05"),
			logs[0].Timestamp.Format("2006-01-02 15:04:05"))
	}

	logger.Printf("[Models] GetGameLogs: 查询成功, 返回 %d 条记录", len(logs))
	return logs, count, nil
}

// 更新日志统计信息
func updateLogStatistics(archiveName, worldName, logType string, timestamp time.Time) error {
	// 获取日期（不包含时间）
	date := time.Date(timestamp.Year(), timestamp.Month(), timestamp.Day(), 0, 0, 0, 0, timestamp.Location())

	// 创建统计键
	key := fmt.Sprintf("%s:%s:%s:%s", archiveName, worldName, logType, date.Format("2006-01-02"))

	// 更新缓存
	statCacheMutex.Lock()
	statCache[key]++

	// 检查是否需要刷新缓存
	cacheSize := len(statCache)
	timeToFlush := time.Since(lastFlushTime) > flushInterval
	statCacheMutex.Unlock()

	// 如果缓存超过阈值或者时间超过间隔，则刷新缓存
	if cacheSize >= cacheThreshold || timeToFlush {
		go FlushStatCache()
	}

	return nil
}

// AddLogExtractRule 添加日志提取规则
func AddLogExtractRule(name, description, logType, pattern string, isRegex, isEnabled bool, priority int, matchMode, tailPattern string, lineCount int) error {
	// 如果未指定匹配模式，默认为单行匹配
	if matchMode == "" {
		matchMode = MatchModeSingle
	}

	rule := LogExtractRule{
		Name:        name,
		Description: description,
		LogType:     logType,
		Pattern:     pattern,
		IsRegex:     isRegex,
		IsEnabled:   isEnabled,
		Priority:    priority,
		MatchMode:   matchMode,
		TailPattern: tailPattern,
		LineCount:   lineCount,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	return db.Create(&rule).Error
}

// UpdateLogExtractRule 更新日志提取规则
func UpdateLogExtractRule(id int, name, description, logType, pattern string, isRegex, isEnabled bool, priority int, matchMode, tailPattern string, lineCount int) error {
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

	// 如果指定了匹配模式，更新匹配模式
	if matchMode != "" {
		updates["match_mode"] = matchMode
	}

	// 如果指定了尾行匹配模式，更新尾行匹配模式
	if tailPattern != "" {
		updates["tail_pattern"] = tailPattern
	}

	// 如果指定了固定行数，更新固定行数
	if lineCount > 0 || matchMode == MatchModeFixedLines {
		updates["line_count"] = lineCount
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
	if err := query.Order("timestamp desc, created_at desc").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}

	// 打印日志时间戳信息以便调试
	if len(logs) > 0 {
		logger.Printf("[Models] SearchGameLogs: 日志时间戳范围: 最早=%s, 最晚=%s",
			logs[len(logs)-1].Timestamp.Format("2006-01-02 15:04:05"),
			logs[0].Timestamp.Format("2006-01-02 15:04:05"))
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

// GetArchivePath 获取存档路径
func GetArchivePath(archiveName string) string {
	// 从配置文件获取DST存档路径
	dstSavePath := "./Klei/DoNotStarveTogether" // 默认路径

	// 尝试从配置文件读取
	configFile := configpath.Current()
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
				dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
			}
		}
	}

	// 构造存档路径
	archivePath := filepath.Join(dstSavePath, archiveName)

	// 检查存档路径是否存在
	if _, err := os.Stat(archivePath); os.IsNotExist(err) {
		logger.Printf("[Models] GetArchivePath 失败: 存档路径不存在, 路径=%s", archivePath)
		return ""
	}

	return archivePath
}

// GetServerLogPath 获取服务器日志文件路径
func GetServerLogPath(archiveName, worldName string) string {
	// 获取存档路径
	archivePath := GetArchivePath(archiveName)
	if archivePath == "" {
		logger.Printf("[Models] GetServerLogPath 失败: 无法获取存档路径, 存档=%s", archiveName)
		return ""
	}

	// 构造日志文件路径
	logPath := filepath.Join(archivePath, worldName, "server_log.txt")

	// 检查文件是否存在
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		logger.Printf("[Models] GetServerLogPath 失败: 日志文件不存在, 路径=%s", logPath)
		return ""
	}

	return logPath
}

// ReadFileHead 读取文件的前n行
func ReadFileHead(filePath string, n int) (string, error) {
	// 打开文件
	file, err := os.Open(filePath)
	if err != nil {
		logger.Printf("[Models] ReadFileHead 失败: 无法打开文件, 路径=%s, 错误=%v", filePath, err)
		return "", err
	}
	defer file.Close()

	// 创建扫描器
	scanner := bufio.NewScanner(file)

	// 读取前n行
	lines := make([]string, 0, n)
	for i := 0; i < n && scanner.Scan(); i++ {
		lines = append(lines, scanner.Text())
	}

	// 检查扫描错误
	if err := scanner.Err(); err != nil {
		logger.Printf("[Models] ReadFileHead 失败: 扫描文件时出错, 路径=%s, 错误=%v", filePath, err)
		return "", err
	}

	// 返回读取的内容
	return strings.Join(lines, "\n"), nil
}

// GetStartupVersions 获取所有启动版本
func GetStartupVersions(archiveName, worldName string) ([]string, error) {
	logger.Printf("[Models] GetStartupVersions: 开始查询启动版本, 存档=%s, 世界=%s",
		archiveName, worldName)

	var versions []string

	// 检查数据库连接
	if db == nil {
		logger.Printf("[Models] GetStartupVersions 失败: 数据库连接为空")
		return nil, fmt.Errorf("数据库连接为空")
	}

	// 直接查询不同的启动版本
	query := db.Model(&GameLog{}).Select("DISTINCT startup_version")

	// 添加查询条件
	if archiveName != "" {
		query = query.Where("archive_name = ?", archiveName)
	}
	if worldName != "" {
		query = query.Where("world_name = ?", worldName)
	}

	// 执行查询
	rows, err := query.Rows()
	if err != nil {
		logger.Printf("[Models] GetStartupVersions 查询失败: %v", err)
		return nil, err
	}
	defer rows.Close()

	// 遍历结果
	type VersionInfo struct {
		Version   string
		Timestamp time.Time
	}
	var versionInfos []VersionInfo

	// 遍历结果
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			logger.Printf("[Models] GetStartupVersions 扫描结果失败: %v", err)
			return nil, err
		}
		if version != "" {
			// 查询每个版本的最新日志时间
			var latestLog GameLog
			if err := db.Where("startup_version = ?", version).
				Order("timestamp DESC").First(&latestLog).Error; err == nil {
				versionInfos = append(versionInfos, VersionInfo{
					Version:   version,
					Timestamp: latestLog.Timestamp,
				})
			} else {
				// 如果无法获取时间戳，仍然添加版本
				versionInfos = append(versionInfos, VersionInfo{
					Version:   version,
					Timestamp: time.Time{},
				})
			}
		}
	}

	// 按时间戳降序排序
	sort.Slice(versionInfos, func(i, j int) bool {
		// 如果两个版本的时间戳都为零值，则按版本名称排序
		if versionInfos[i].Timestamp.IsZero() && versionInfos[j].Timestamp.IsZero() {
			return versionInfos[i].Version > versionInfos[j].Version
		}
		// 如果一个版本的时间戳为零值，另一个不为零值，则非零值的排在前面
		if versionInfos[i].Timestamp.IsZero() {
			return false
		}
		if versionInfos[j].Timestamp.IsZero() {
			return true
		}
		// 如果两个版本的时间戳都不为零值，则按时间戳降序排序
		return versionInfos[i].Timestamp.After(versionInfos[j].Timestamp)
	})

	// 提取版本列表
	for _, info := range versionInfos {
		versions = append(versions, info.Version)
		if !info.Timestamp.IsZero() {
			logger.Printf("[Models] GetStartupVersions: 版本 %s, 最新时间 %s",
				info.Version, info.Timestamp.Format("2006-01-02 15:04:05"))
		} else {
			logger.Printf("[Models] GetStartupVersions: 版本 %s, 无时间戳信息", info.Version)
		}
	}

	logger.Printf("[Models] GetStartupVersions: 查询成功，返回 %d 个启动版本", len(versions))
	return versions, nil
}

// CloseStatCache 关闭统计缓存，确保所有缓存数据被写入数据库
func CloseStatCache() {
	// 刷新缓存到数据库
	FlushStatCache()
	logger.Printf("[Models] 统计缓存已关闭并刷新到数据库")
}

// ArchiveWorldInfo 存档和世界信息
type ArchiveWorldInfo struct {
	ArchiveName string   `json:"archive_name"` // 存档名称
	Worlds      []string `json:"worlds"`       // 世界列表
}

// GetArchivesWithLogs 获取有日志的存档和世界列表
func GetArchivesWithLogs() ([]ArchiveWorldInfo, error) {
	logger.Printf("[Models] GetArchivesWithLogs: 开始查询有日志的存档和世界列表")

	// 检查数据库连接
	if db == nil {
		logger.Printf("[Models] GetArchivesWithLogs 失败: 数据库连接为空")
		return nil, fmt.Errorf("数据库连接为空")
	}

	// 查询所有唯一的存档名称
	var archiveNames []string
	query := db.Model(&LogStatistics{}).Select("DISTINCT archive_name")
	rows, err := query.Rows()
	if err != nil {
		logger.Printf("[Models] GetArchivesWithLogs 查询存档名称失败: %v", err)
		return nil, err
	}
	defer rows.Close()

	// 遍历结果
	for rows.Next() {
		var archiveName string
		if err := rows.Scan(&archiveName); err != nil {
			logger.Printf("[Models] GetArchivesWithLogs 扫描存档名称失败: %v", err)
			return nil, err
		}
		if archiveName != "" {
			archiveNames = append(archiveNames, archiveName)
		}
	}

	// 为每个存档查询世界列表
	result := make([]ArchiveWorldInfo, 0, len(archiveNames))
	for _, archiveName := range archiveNames {
		// 查询该存档下的所有唯一世界名称
		var worldNames []string
		worldQuery := db.Model(&LogStatistics{}).Select("DISTINCT world_name").Where("archive_name = ?", archiveName)
		worldRows, err := worldQuery.Rows()
		if err != nil {
			logger.Printf("[Models] GetArchivesWithLogs 查询世界名称失败: %v", err)
			continue
		}

		// 遍历结果
		for worldRows.Next() {
			var worldName string
			if err := worldRows.Scan(&worldName); err != nil {
				logger.Printf("[Models] GetArchivesWithLogs 扫描世界名称失败: %v", err)
				worldRows.Close()
				continue
			}
			if worldName != "" {
				worldNames = append(worldNames, worldName)
			}
		}
		worldRows.Close()

		// 添加到结果中
		if len(worldNames) > 0 {
			result = append(result, ArchiveWorldInfo{
				ArchiveName: archiveName,
				Worlds:      worldNames,
			})
		}
	}

	logger.Printf("[Models] GetArchivesWithLogs: 查询成功，返回 %d 个存档信息", len(result))
	return result, nil
}
