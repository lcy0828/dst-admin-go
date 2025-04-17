package models

import (
	"time"
)

// TriggerType 触发类型
type TriggerType int

const (
	TriggerAuto   TriggerType = 0 // 自动触发（定时任务）
	TriggerManual TriggerType = 1 // 手动触发
)

// CronTaskLog 定时任务执行日志
type CronTaskLog struct {
	ID          int         `gorm:"primary_key" json:"id"`
	TaskID      int         `json:"task_id"`      // 任务ID
	TaskName    string      `json:"task_name"`    // 任务名称
	Status      int         `json:"status"`       // 执行状态：0-失败，1-成功
	Output      string      `json:"output"`       // 执行输出
	Error       string      `json:"error"`        // 错误信息
	StartTime   time.Time   `json:"start_time"`   // 开始时间
	EndTime     time.Time   `json:"end_time"`     // 结束时间
	Duration    int64       `json:"duration"`     // 执行时长（毫秒）
	TriggerType TriggerType `json:"trigger_type"` // 触发类型：0-自动触发，1-手动触发
	CreatedAt   time.Time   `json:"created_at"`   // 创建时间
}

// AddTaskLog 添加任务日志
func AddTaskLog(log *CronTaskLog) error {
	return db.Create(log).Error
}

// GetTaskLogs 获取任务日志列表
func GetTaskLogs(taskID int, page, pageSize int) ([]CronTaskLog, int, error) {
	var logs []CronTaskLog
	var count int64

	// 构建查询
	query := db
	if taskID > 0 {
		query = query.Where("task_id = ?", taskID)
	}

	// 获取总数
	err := query.Model(&CronTaskLog{}).Count(&count).Error
	if err != nil {
		return nil, 0, err
	}

	// 分页查询
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}
	offset := (page - 1) * pageSize

	// 获取数据
	err = query.Order("id DESC").Offset(offset).Limit(pageSize).Find(&logs).Error
	if err != nil {
		return nil, 0, err
	}

	return logs, int(count), nil
}

// GetTaskLogByID 根据ID获取任务日志
func GetTaskLogByID(id int) (*CronTaskLog, error) {
	var log CronTaskLog
	err := db.Where("id = ?", id).First(&log).Error
	if err != nil {
		return nil, err
	}
	return &log, nil
}

// ClearOldTaskLogs 清理指定日期之前的任务日志
func ClearOldTaskLogs(retentionDate time.Time) error {
	return db.Where("created_at < ?", retentionDate).Delete(&CronTaskLog{}).Error
}

// GetRecentTaskLogs 获取最近的任务日志
func GetRecentTaskLogs(limit int) ([]CronTaskLog, error) {
	var logs []CronTaskLog
	err := db.Order("id DESC").Limit(limit).Find(&logs).Error
	if err != nil {
		return nil, err
	}
	return logs, nil
}

// GetTaskLogStats 获取任务日志统计信息
func GetTaskLogStats(taskID int, days int) (map[string]interface{}, error) {
	// 计算开始日期
	startDate := time.Now().AddDate(0, 0, -days)

	// 统计总执行次数
	var totalCount int64
	err := db.Model(&CronTaskLog{}).Where("task_id = ? AND created_at >= ?", taskID, startDate).Count(&totalCount).Error
	if err != nil {
		return nil, err
	}

	// 统计成功次数
	var successCount int64
	err = db.Model(&CronTaskLog{}).Where("task_id = ? AND status = 1 AND created_at >= ?", taskID, startDate).Count(&successCount).Error
	if err != nil {
		return nil, err
	}

	// 统计失败次数
	var failCount int64
	err = db.Model(&CronTaskLog{}).Where("task_id = ? AND status = 0 AND created_at >= ?", taskID, startDate).Count(&failCount).Error
	if err != nil {
		return nil, err
	}

	// 计算平均执行时长
	var avgDuration float64
	err = db.Model(&CronTaskLog{}).Where("task_id = ? AND created_at >= ?", taskID, startDate).Select("AVG(duration)").Row().Scan(&avgDuration)
	if err != nil {
		avgDuration = 0
	}

	// 获取最近一次执行状态
	var lastLog CronTaskLog
	err = db.Where("task_id = ?", taskID).Order("id DESC").First(&lastLog).Error
	lastStatus := -1
	lastRunTime := time.Time{}
	if err == nil {
		lastStatus = lastLog.Status
		lastRunTime = lastLog.StartTime
	}

	// 返回统计结果
	return map[string]interface{}{
		"total_count":   totalCount,
		"success_count": successCount,
		"fail_count":    failCount,
		"avg_duration":  avgDuration,
		"last_status":   lastStatus,
		"last_run_time": lastRunTime,
	}, nil
}

// InitCronTaskLogTable 初始化定时任务日志表
func InitCronTaskLogTable() {
	db.AutoMigrate(&CronTaskLog{})
}

// GetTriggerTypeName 获取触发类型名称
func GetTriggerTypeName(triggerType TriggerType) string {
	switch triggerType {
	case TriggerAuto:
		return "自动触发"
	case TriggerManual:
		return "手动触发"
	default:
		return "未知触发类型"
	}
}
