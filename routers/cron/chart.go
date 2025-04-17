package cron

import (
	"dont/models"
	"dont/pkg/e"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// RegisterCronChartRoutes 注册定时任务图表相关路由
func RegisterCronChartRoutes(router *gin.RouterGroup) {
	cronChartGroup := router.Group("/cron/chart")
	{
		// 获取任务执行图表数据
		cronChartGroup.GET("/task/:id", GetTaskExecutionChart)

		// 获取任务执行时长图表数据
		cronChartGroup.GET("/task/:id/duration", GetTaskDurationChart)

		// 获取所有任务组执行图表数据
		cronChartGroup.GET("/groups", GetAllGroupsExecutionChart)

		// 获取任务组执行图表数据
		cronChartGroup.GET("/group/:id", GetGroupExecutionChart)

		// 获取系统概览图表数据
		cronChartGroup.GET("/overview", GetSystemOverviewChart)
	}
}

// GetTaskExecutionChart 获取任务执行图表数据
func GetTaskExecutionChart(c *gin.Context) {
	idStr := c.Param("id")
	daysStr := c.DefaultQuery("days", "7")

	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	days, err := strconv.Atoi(daysStr)
	if err != nil || days <= 0 {
		days = 7
	}

	// 获取任务执行图表数据
	chart, err := models.GetTaskExecutionChart(id, days)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务执行图表数据失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": chart,
	})
}

// GetTaskDurationChart 获取任务执行时长图表数据
func GetTaskDurationChart(c *gin.Context) {
	idStr := c.Param("id")
	daysStr := c.DefaultQuery("days", "7")

	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	days, err := strconv.Atoi(daysStr)
	if err != nil || days <= 0 {
		days = 7
	}

	// 获取任务执行时长图表数据
	chart, err := models.GetTaskDurationChart(id, days)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务执行时长图表数据失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": chart,
	})
}

// GetAllGroupsExecutionChart 获取所有任务组执行图表数据
func GetAllGroupsExecutionChart(c *gin.Context) {
	daysStr := c.DefaultQuery("days", "7")

	days, err := strconv.Atoi(daysStr)
	if err != nil || days <= 0 {
		days = 7
	}

	// 获取所有任务组执行图表数据
	charts, err := models.GetAllGroupsExecutionChart(days)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取所有任务组执行图表数据失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": charts,
	})
}

// GetGroupExecutionChart 获取任务组执行图表数据
func GetGroupExecutionChart(c *gin.Context) {
	idStr := c.Param("id")
	daysStr := c.DefaultQuery("days", "7")

	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组ID",
			"data": nil,
		})
		return
	}

	days, err := strconv.Atoi(daysStr)
	if err != nil || days <= 0 {
		days = 7
	}

	// 获取任务组执行图表数据
	chart, err := models.GetGroupExecutionChart(id, days)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务组执行图表数据失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": chart,
	})
}

// SystemOverviewChart 系统概览图表数据
type SystemOverviewChart struct {
	TaskCount      int                       `json:"task_count"`      // 任务总数
	GroupCount     int                       `json:"group_count"`     // 任务组总数
	EnabledCount   int                       `json:"enabled_count"`   // 启用的任务数
	DisabledCount  int                       `json:"disabled_count"`  // 禁用的任务数
	SuccessCount   int                       `json:"success_count"`   // 成功执行的任务数
	FailCount      int                       `json:"fail_count"`      // 失败执行的任务数
	NeverRunCount  int                       `json:"never_run_count"` // 从未运行的任务数
	GroupStats     []GroupStat               `json:"group_stats"`     // 任务组统计
	ExecutionChart models.TaskExecutionChart `json:"execution_chart"` // 执行图表数据
}

// GroupStat 任务组统计
type GroupStat struct {
	GroupID   int    `json:"group_id"`   // 任务组ID
	GroupName string `json:"group_name"` // 任务组名称
	TaskCount int    `json:"task_count"` // 任务数
}

// GetSystemOverviewChart 获取系统概览图表数据
func GetSystemOverviewChart(c *gin.Context) {
	daysStr := c.DefaultQuery("days", "7")

	days, err := strconv.Atoi(daysStr)
	if err != nil || days <= 0 {
		days = 7
	}

	// 获取所有任务
	tasks, err := models.GetAllTasks()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 获取所有任务组
	groups, err := models.GetAllTaskGroups()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务组失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 统计数据
	overview := SystemOverviewChart{
		TaskCount:     len(tasks),
		GroupCount:    len(groups),
		EnabledCount:  0,
		DisabledCount: 0,
		SuccessCount:  0,
		FailCount:     0,
		NeverRunCount: 0,
		GroupStats:    make([]GroupStat, 0, len(groups)),
	}

	// 任务组统计
	groupTaskCount := make(map[int]int)
	for _, task := range tasks {
		groupTaskCount[task.GroupID]++

		if task.Status == 1 {
			overview.EnabledCount++
		} else {
			overview.DisabledCount++
		}

		if task.LastRunTime.IsZero() {
			overview.NeverRunCount++
		} else if task.LastStatus == 1 {
			overview.SuccessCount++
		} else {
			overview.FailCount++
		}
	}

	// 填充任务组统计
	for _, group := range groups {
		overview.GroupStats = append(overview.GroupStats, GroupStat{
			GroupID:   group.ID,
			GroupName: group.Name,
			TaskCount: groupTaskCount[group.ID],
		})
	}

	// 获取系统执行图表数据
	// 这里简单地获取所有任务的执行数据，实际上可能需要更复杂的查询
	startDate := time.Now().AddDate(0, 0, -days)

	// 初始化图表数据
	overview.ExecutionChart = models.TaskExecutionChart{
		Success: make([]models.ChartDataPoint, 0, days),
		Fail:    make([]models.ChartDataPoint, 0, days),
		Total:   make([]models.ChartDataPoint, 0, days),
	}

	// 初始化日期映射
	dateMap := make(map[string]int)
	for i := 0; i < days; i++ {
		date := startDate.AddDate(0, 0, i).Format("2006-01-02")
		dateMap[date] = i

		// 初始化数据点
		overview.ExecutionChart.Success = append(overview.ExecutionChart.Success, models.ChartDataPoint{Date: date, Count: 0})
		overview.ExecutionChart.Fail = append(overview.ExecutionChart.Fail, models.ChartDataPoint{Date: date, Count: 0})
		overview.ExecutionChart.Total = append(overview.ExecutionChart.Total, models.ChartDataPoint{Date: date, Count: 0})
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
		FROM dont_cron_task_log
		WHERE status = 1 AND start_time >= ?
		GROUP BY DATE(start_time)
	`
	if err := models.DB().Raw(query, startDate).Scan(&successResults).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取执行图表数据失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 填充成功执行数据
	for _, result := range successResults {
		if idx, ok := dateMap[result.Date]; ok {
			overview.ExecutionChart.Success[idx].Count = result.Count
			overview.ExecutionChart.Total[idx].Count += result.Count
		}
	}

	// 查询失败执行数据
	var failResults []Result
	query = `
		SELECT DATE(start_time) as date, COUNT(*) as count
		FROM dont_cron_task_log
		WHERE status = 0 AND start_time >= ?
		GROUP BY DATE(start_time)
	`
	if err := models.DB().Raw(query, startDate).Scan(&failResults).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取执行图表数据失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 填充失败执行数据
	for _, result := range failResults {
		if idx, ok := dateMap[result.Date]; ok {
			overview.ExecutionChart.Fail[idx].Count = result.Count
			overview.ExecutionChart.Total[idx].Count += result.Count
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": overview,
	})
}
