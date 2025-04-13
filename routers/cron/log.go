package cron

import (
	"dont/models"
	"dont/pkg/e"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// RegisterCronLogRoutes 注册定时任务日志相关路由
func RegisterCronLogRoutes(router *gin.RouterGroup) {
	cronLogGroup := router.Group("/cron/logs")
	{
		// 获取任务日志列表
		cronLogGroup.GET("", GetTaskLogs)

		// 获取任务日志详情
		cronLogGroup.GET("/:id", GetTaskLogByID)

		// 获取任务统计信息
		cronLogGroup.GET("/stats/:task_id", GetTaskLogStats)

		// 清理旧日志
		cronLogGroup.POST("/clear", ClearOldTaskLogs)

		// 获取最近的任务日志
		cronLogGroup.GET("/recent", GetRecentTaskLogs)
	}
}

// GetTaskLogs 获取任务日志列表
func GetTaskLogs(c *gin.Context) {
	// 获取查询参数
	taskIDStr := c.Query("task_id")
	pageStr := c.DefaultQuery("page", "1")
	pageSizeStr := c.DefaultQuery("page_size", "10")

	// 转换参数
	taskID := 0
	if taskIDStr != "" {
		var err error
		taskID, err = strconv.Atoi(taskIDStr)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "无效的任务ID",
				"data": nil,
			})
			return
		}
	}

	page, err := strconv.Atoi(pageStr)
	if err != nil {
		page = 1
	}

	pageSize, err := strconv.Atoi(pageSizeStr)
	if err != nil {
		pageSize = 10
	}

	// 获取日志列表
	logs, total, err := models.GetTaskLogs(taskID, page, pageSize)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务日志失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": gin.H{
			"logs":       logs,
			"total":      total,
			"page":       page,
			"page_size":  pageSize,
			"total_page": (total + pageSize - 1) / pageSize,
		},
	})
}

// GetTaskLogByID 获取任务日志详情
func GetTaskLogByID(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的日志ID",
			"data": nil,
		})
		return
	}

	log, err := models.GetTaskLogByID(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务日志失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": log,
	})
}

// GetTaskLogStats 获取任务统计信息
func GetTaskLogStats(c *gin.Context) {
	taskIDStr := c.Param("task_id")
	daysStr := c.DefaultQuery("days", "7")

	taskID, err := strconv.Atoi(taskIDStr)
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

	stats, err := models.GetTaskLogStats(taskID, days)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务统计信息失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": stats,
	})
}

// ClearOldTaskLogsRequest 清理旧日志请求
type ClearOldTaskLogsRequest struct {
	Days int `json:"days" binding:"required"`
}

// ClearOldTaskLogs 清理旧日志
func ClearOldTaskLogs(c *gin.Context) {
	var req ClearOldTaskLogsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	if req.Days <= 0 {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "天数必须大于0",
			"data": nil,
		})
		return
	}

	// 计算保留日期
	retentionDate := time.Now().AddDate(0, 0, -req.Days)

	// 清理旧日志
	if err := models.ClearOldTaskLogs(retentionDate); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "清理旧日志失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "清理旧日志成功",
		"data": gin.H{
			"retention_date": retentionDate.Format("2006-01-02"),
		},
	})
}

// GetRecentTaskLogs 获取最近的任务日志
func GetRecentTaskLogs(c *gin.Context) {
	limitStr := c.DefaultQuery("limit", "10")

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		limit = 10
	}

	logs, err := models.GetRecentTaskLogs(limit)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取最近任务日志失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": logs,
	})
}
