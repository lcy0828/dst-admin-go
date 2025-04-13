package models

import (
	"time"
)

// ChartDataPoint 图表数据点
type ChartDataPoint struct {
	Date  string `json:"date"`  // 日期，格式：YYYY-MM-DD
	Count int    `json:"count"` // 数量
}

// TaskExecutionChart 任务执行图表数据
type TaskExecutionChart struct {
	Success []ChartDataPoint `json:"success"` // 成功执行数据
	Fail    []ChartDataPoint `json:"fail"`    // 失败执行数据
	Total   []ChartDataPoint `json:"total"`   // 总执行数据
}

// GroupExecutionChart 任务组执行图表数据
type GroupExecutionChart struct {
	GroupID   int                `json:"group_id"`   // 任务组ID
	GroupName string             `json:"group_name"` // 任务组名称
	Data      TaskExecutionChart `json:"data"`       // 图表数据
}

// GetTaskExecutionChart 获取任务执行图表数据
func GetTaskExecutionChart(taskID int, days int) (*TaskExecutionChart, error) {
	// 计算开始日期
	startDate := time.Now().AddDate(0, 0, -days)

	// 初始化图表数据
	chart := &TaskExecutionChart{
		Success: make([]ChartDataPoint, 0, days),
		Fail:    make([]ChartDataPoint, 0, days),
		Total:   make([]ChartDataPoint, 0, days),
	}

	// 初始化日期映射
	dateMap := make(map[string]int)
	for i := 0; i < days; i++ {
		date := startDate.AddDate(0, 0, i).Format("2006-01-02")
		dateMap[date] = i

		// 初始化数据点
		chart.Success = append(chart.Success, ChartDataPoint{Date: date, Count: 0})
		chart.Fail = append(chart.Fail, ChartDataPoint{Date: date, Count: 0})
		chart.Total = append(chart.Total, ChartDataPoint{Date: date, Count: 0})
	}

	// 查询成功执行数据
	type Result struct {
		Date  string
		Count int
	}

	// 查询成功执行数据
	var successResults []Result
	query := `
		SELECT DATE(start_time) as date, COUNT(*) as count
		FROM cron_task_log
		WHERE task_id = ? AND status = 1 AND start_time >= ?
		GROUP BY DATE(start_time)
	`
	if err := db.Raw(query, taskID, startDate).Scan(&successResults).Error; err != nil {
		return nil, err
	}

	// 填充成功执行数据
	for _, result := range successResults {
		if idx, ok := dateMap[result.Date]; ok {
			chart.Success[idx].Count = result.Count
			chart.Total[idx].Count += result.Count
		}
	}

	// 查询失败执行数据
	var failResults []Result
	query = `
		SELECT DATE(start_time) as date, COUNT(*) as count
		FROM cron_task_log
		WHERE task_id = ? AND status = 0 AND start_time >= ?
		GROUP BY DATE(start_time)
	`
	if err := db.Raw(query, taskID, startDate).Scan(&failResults).Error; err != nil {
		return nil, err
	}

	// 填充失败执行数据
	for _, result := range failResults {
		if idx, ok := dateMap[result.Date]; ok {
			chart.Fail[idx].Count = result.Count
			chart.Total[idx].Count += result.Count
		}
	}

	return chart, nil
}

// GetGroupExecutionChart 获取任务组执行图表数据
func GetGroupExecutionChart(groupID int, days int) (*GroupExecutionChart, error) {
	// 获取任务组信息
	group, err := GetTaskGroup(groupID)
	if err != nil {
		return nil, err
	}

	// 计算开始日期
	startDate := time.Now().AddDate(0, 0, -days)

	// 初始化图表数据
	chart := &GroupExecutionChart{
		GroupID:   groupID,
		GroupName: group.Name,
		Data: TaskExecutionChart{
			Success: make([]ChartDataPoint, 0, days),
			Fail:    make([]ChartDataPoint, 0, days),
			Total:   make([]ChartDataPoint, 0, days),
		},
	}

	// 初始化日期映射
	dateMap := make(map[string]int)
	for i := 0; i < days; i++ {
		date := startDate.AddDate(0, 0, i).Format("2006-01-02")
		dateMap[date] = i

		// 初始化数据点
		chart.Data.Success = append(chart.Data.Success, ChartDataPoint{Date: date, Count: 0})
		chart.Data.Fail = append(chart.Data.Fail, ChartDataPoint{Date: date, Count: 0})
		chart.Data.Total = append(chart.Data.Total, ChartDataPoint{Date: date, Count: 0})
	}

	// 查询成功执行数据
	type Result struct {
		Date  string
		Count int
	}

	// 查询成功执行数据
	var successResults []Result
	query := `
		SELECT DATE(l.start_time) as date, COUNT(*) as count
		FROM cron_task_log l
		JOIN cron_task t ON l.task_id = t.id
		WHERE t.group_id = ? AND l.status = 1 AND l.start_time >= ?
		GROUP BY DATE(l.start_time)
	`
	if err := db.Raw(query, groupID, startDate).Scan(&successResults).Error; err != nil {
		return nil, err
	}

	// 填充成功执行数据
	for _, result := range successResults {
		if idx, ok := dateMap[result.Date]; ok {
			chart.Data.Success[idx].Count = result.Count
			chart.Data.Total[idx].Count += result.Count
		}
	}

	// 查询失败执行数据
	var failResults []Result
	query = `
		SELECT DATE(l.start_time) as date, COUNT(*) as count
		FROM cron_task_log l
		JOIN cron_task t ON l.task_id = t.id
		WHERE t.group_id = ? AND l.status = 0 AND l.start_time >= ?
		GROUP BY DATE(l.start_time)
	`
	if err := db.Raw(query, groupID, startDate).Scan(&failResults).Error; err != nil {
		return nil, err
	}

	// 填充失败执行数据
	for _, result := range failResults {
		if idx, ok := dateMap[result.Date]; ok {
			chart.Data.Fail[idx].Count = result.Count
			chart.Data.Total[idx].Count += result.Count
		}
	}

	return chart, nil
}

// GetAllGroupsExecutionChart 获取所有任务组执行图表数据
func GetAllGroupsExecutionChart(days int) ([]GroupExecutionChart, error) {
	// 获取所有任务组
	groups, err := GetAllTaskGroups()
	if err != nil {
		return nil, err
	}

	// 初始化结果
	charts := make([]GroupExecutionChart, 0, len(groups))

	// 获取每个任务组的图表数据
	for _, group := range groups {
		chart, err := GetGroupExecutionChart(group.ID, days)
		if err != nil {
			continue
		}
		charts = append(charts, *chart)
	}

	return charts, nil
}

// GetTaskDurationChart 获取任务执行时长图表数据
func GetTaskDurationChart(taskID int, days int) ([]ChartDataPoint, error) {
	// 计算开始日期
	startDate := time.Now().AddDate(0, 0, -days)

	// 初始化图表数据
	chart := make([]ChartDataPoint, 0, days)

	// 初始化日期映射
	dateMap := make(map[string]int)
	for i := 0; i < days; i++ {
		date := startDate.AddDate(0, 0, i).Format("2006-01-02")
		dateMap[date] = i

		// 初始化数据点
		chart = append(chart, ChartDataPoint{Date: date, Count: 0})
	}

	// 查询平均执行时长数据
	type Result struct {
		Date     string
		Duration int
	}

	var results []Result
	query := `
		SELECT DATE(start_time) as date, AVG(duration) as duration
		FROM cron_task_log
		WHERE task_id = ? AND start_time >= ?
		GROUP BY DATE(start_time)
	`
	if err := db.Raw(query, taskID, startDate).Scan(&results).Error; err != nil {
		return nil, err
	}

	// 填充执行时长数据
	for _, result := range results {
		if idx, ok := dateMap[result.Date]; ok {
			chart[idx].Count = result.Duration
		}
	}

	return chart, nil
}
