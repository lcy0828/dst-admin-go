package logparser

import (
	"dont/models"
	"fmt"
	"log"
	"sync"
	"time"
)

// LogParserManager 日志解析器管理器
type LogParserManager struct {
	parsers      map[string]*LogParser // 解析器映射，键为"存档名_世界名"
	parsersMutex sync.RWMutex          // 解析器读写锁
}

var (
	manager     *LogParserManager
	managerOnce sync.Once
)

// GetLogParserManager 获取日志解析器管理器单例
func GetLogParserManager() *LogParserManager {
	managerOnce.Do(func() {
		manager = &LogParserManager{
			parsers: make(map[string]*LogParser),
		}
	})
	return manager
}

// GetParser 获取指定存档和世界的日志解析器
func (m *LogParserManager) GetParser(archiveName, worldName string) (*LogParser, error) {
	key := fmt.Sprintf("%s_%s", archiveName, worldName)

	// 先尝试从缓存获取
	m.parsersMutex.RLock()
	parser, exists := m.parsers[key]
	m.parsersMutex.RUnlock()

	if exists {
		return parser, nil
	}

	// 如果不存在，创建新的解析器
	m.parsersMutex.Lock()
	defer m.parsersMutex.Unlock()

	// 再次检查，防止在获取锁的过程中被其他goroutine创建
	parser, exists = m.parsers[key]
	if exists {
		return parser, nil
	}

	// 创建新的解析器
	newParser, err := NewLogParser(archiveName, worldName)
	if err != nil {
		return nil, err
	}

	m.parsers[key] = newParser
	return newParser, nil
}

// ReloadAllParsers 重新加载所有解析器的规则
func (m *LogParserManager) ReloadAllParsers() error {
	m.parsersMutex.RLock()
	defer m.parsersMutex.RUnlock()

	for _, parser := range m.parsers {
		if err := parser.ReloadRules(); err != nil {
			return err
		}
	}

	return nil
}

// ProcessLog 处理日志内容
func (m *LogParserManager) ProcessLog(archiveName, worldName, content string) error {
	parser, err := m.GetParser(archiveName, worldName)
	if err != nil {
		return err
	}

	return parser.ProcessAndSaveLog(content)
}

// StartCleanupTask 启动日志清理任务
func (m *LogParserManager) StartCleanupTask(retentionDays int) {
	go func() {
		ticker := time.NewTicker(24 * time.Hour) // 每天执行一次
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// 计算保留日期
				retentionDate := time.Now().AddDate(0, 0, -retentionDays)

				// 清理旧日志
				if err := models.ClearOldLogs(retentionDate); err != nil {
					log.Printf("清理旧日志失败: %v", err)
				} else {
					log.Printf("已清理 %s 之前的日志", retentionDate.Format("2006-01-02"))
				}
			}
		}
	}()
}

// GetLogStatistics 获取日志统计信息
func (m *LogParserManager) GetLogStatistics(archiveName, worldName string, days int) (map[string]map[string]int, error) {
	// 计算开始日期
	startDate := time.Now().AddDate(0, 0, -days)
	endDate := time.Now()

	// 获取统计数据
	stats, err := models.GetLogStatistics(archiveName, worldName, startDate, endDate)
	if err != nil {
		return nil, err
	}

	// 按日期和类型组织数据
	result := make(map[string]map[string]int)
	for _, stat := range stats {
		dateStr := stat.Date.Format("2006-01-02")
		if _, ok := result[dateStr]; !ok {
			result[dateStr] = make(map[string]int)
		}
		result[dateStr][stat.LogType] = stat.Count
	}

	return result, nil
}

// GetRecentLogs 获取最近的日志
func (m *LogParserManager) GetRecentLogs(archiveName, worldName string, logType string, limit int) ([]models.GameLog, error) {
	// 获取最近的日志
	logs, _, err := models.GetGameLogs(archiveName, worldName, logType, "", time.Time{}, time.Now(), 1, limit)
	return logs, err
}

// SearchLogs 搜索日志
func (m *LogParserManager) SearchLogs(keyword string, page, pageSize int) ([]models.GameLog, int, error) {
	return models.SearchGameLogs(keyword, page, pageSize)
}

// GetLogTypeDistribution 获取日志类型分布
func (m *LogParserManager) GetLogTypeDistribution(archiveName, worldName string) (map[string]int, error) {
	return models.GetLogTypeCount(archiveName, worldName)
}

// GetStartupVersions 获取启动版本列表
func (m *LogParserManager) GetStartupVersions(archiveName, worldName string) ([]string, error) {
	return models.GetStartupVersions(archiveName, worldName)
}

// AddCustomRule 添加自定义规则
func (m *LogParserManager) AddCustomRule(name, description, logType, pattern string, isRegex, isEnabled bool, priority int, matchMode, tailPattern string, lineCount int) error {
	err := models.AddLogExtractRule(name, description, logType, pattern, isRegex, isEnabled, priority, matchMode, tailPattern, lineCount)
	if err != nil {
		return err
	}

	// 重新加载所有解析器的规则
	return m.ReloadAllParsers()
}

// UpdateCustomRule 更新自定义规则
func (m *LogParserManager) UpdateCustomRule(id int, name, description, logType, pattern string, isRegex, isEnabled bool, priority int, matchMode, tailPattern string, lineCount int) error {
	err := models.UpdateLogExtractRule(id, name, description, logType, pattern, isRegex, isEnabled, priority, matchMode, tailPattern, lineCount)
	if err != nil {
		return err
	}

	// 重新加载所有解析器的规则
	return m.ReloadAllParsers()
}

// DeleteCustomRule 删除自定义规则
func (m *LogParserManager) DeleteCustomRule(id int) error {
	err := models.DeleteLogExtractRule(id)
	if err != nil {
		return err
	}

	// 重新加载所有解析器的规则
	return m.ReloadAllParsers()
}

// GetAllRules 获取所有规则
func (m *LogParserManager) GetAllRules() ([]models.LogExtractRule, error) {
	return models.GetLogExtractRules()
}

// GetArchivesWithLogs 获取有日志的存档和世界列表
func (m *LogParserManager) GetArchivesWithLogs() ([]models.ArchiveWorldInfo, error) {
	return models.GetArchivesWithLogs()
}

// ShutdownAllParsers 关闭所有解析器，确保所有缓冲区中的日志都被写入数据库
func (m *LogParserManager) ShutdownAllParsers() {
	m.parsersMutex.Lock()
	defer m.parsersMutex.Unlock()

	log.Printf("[LogParserManager] 开始关闭所有日志解析器，共 %d 个", len(m.parsers))

	for key, parser := range m.parsers {
		if err := parser.Close(); err != nil {
			log.Printf("[LogParserManager] 关闭解析器 %s 失败: %v", key, err)
		} else {
			log.Printf("[LogParserManager] 关闭解析器 %s 成功", key)
		}
	}

	log.Printf("[LogParserManager] 所有日志解析器已关闭")
}
