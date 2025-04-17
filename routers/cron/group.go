package cron

import (
	"dont/cron"
	"dont/models"
	"dont/pkg/e"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// RegisterCronGroupRoutes 注册定时任务组相关路由
func RegisterCronGroupRoutes(router *gin.RouterGroup) {
	cronGroupGroup := router.Group("/cron/groups")
	{
		// 获取所有任务组
		cronGroupGroup.GET("", GetAllTaskGroups)

		// 获取任务组详情
		cronGroupGroup.GET("/:id", GetTaskGroupByID)

		// 添加任务组
		cronGroupGroup.POST("", AddTaskGroup)

		// 更新任务组
		cronGroupGroup.PUT("/:id", UpdateTaskGroup)

		// 删除任务组
		cronGroupGroup.DELETE("/:id", DeleteTaskGroup)

		// 启用任务组
		cronGroupGroup.POST("/:id/enable", EnableTaskGroup)

		// 禁用任务组
		cronGroupGroup.POST("/:id/disable", DisableTaskGroup)

		// 获取任务组下的任务
		cronGroupGroup.GET("/:id/tasks", GetTasksByGroup)

		// 获取任务组统计信息
		cronGroupGroup.GET("/:id/stats", GetTaskGroupStats)

		// 获取任务组图表数据
		cronGroupGroup.GET("/:id/chart", GetTaskGroupChart)
	}
}

// GetAllTaskGroups 获取所有任务组
func GetAllTaskGroups(c *gin.Context) {
	groups, err := models.GetAllTaskGroups()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务组列表失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": groups,
	})
}

// GetTaskGroupByID 获取任务组详情
func GetTaskGroupByID(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组ID",
			"data": nil,
		})
		return
	}

	group, err := models.GetTaskGroup(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务组失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": group,
	})
}

// TaskGroupRequest 任务组请求
type TaskGroupRequest struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
	Type        string `json:"type" binding:"required"`
	Status      int    `json:"status"`
}

// AddTaskGroup 添加任务组
func AddTaskGroup(c *gin.Context) {
	var req TaskGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 验证任务组类型
	if req.Type != "system" && req.Type != "world" && req.Type != "custom" {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组类型，只支持 system、world 和 custom",
			"data": nil,
		})
		return
	}

	// 创建任务组
	group := &models.CronTaskGroup{
		Name:        req.Name,
		Description: req.Description,
		Type:        req.Type,
		Status:      req.Status,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// 添加任务组
	if err := models.AddTaskGroup(group); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "添加任务组失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "添加任务组成功",
		"data": group,
	})
}

// UpdateTaskGroup 更新任务组
func UpdateTaskGroup(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组ID",
			"data": nil,
		})
		return
	}

	// 获取现有任务组
	existingGroup, err := models.GetTaskGroup(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "任务组不存在: " + err.Error(),
			"data": nil,
		})
		return
	}

	var req TaskGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 验证任务组类型
	if req.Type != "system" && req.Type != "world" && req.Type != "custom" {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组类型，只支持 system、world 和 custom",
			"data": nil,
		})
		return
	}

	// 更新任务组
	existingGroup.Name = req.Name
	existingGroup.Description = req.Description
	existingGroup.Type = req.Type
	existingGroup.Status = req.Status
	existingGroup.UpdatedAt = time.Now()

	// 更新任务组
	if err := models.UpdateTaskGroup(existingGroup); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "更新任务组失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 如果状态发生变化，重启任务管理器
	taskManager := cron.GetTaskManager()
	if err := taskManager.Restart(); err != nil {
		log.Printf("更新任务组后重启任务管理器失败: %v", err)
		// 不返回错误，因为任务组状态已经更新成功
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "更新任务组成功",
		"data": existingGroup,
	})
}

// DeleteTaskGroup 删除任务组
func DeleteTaskGroup(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组ID",
			"data": nil,
		})
		return
	}

	// 删除任务组
	if err := models.DeleteTaskGroup(id); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "删除任务组失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "删除任务组成功",
		"data": nil,
	})
}

// EnableTaskGroup 启用任务组
func EnableTaskGroup(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组ID",
			"data": nil,
		})
		return
	}

	// 启用任务组
	if err := models.UpdateTaskGroupStatus(id, 1); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "启用任务组失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 重启任务管理器，使变更生效
	taskManager := cron.GetTaskManager()
	if err := taskManager.Restart(); err != nil {
		log.Printf("启用任务组后重启任务管理器失败: %v", err)
		// 不返回错误，因为任务组状态已经更新成功
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "启用任务组成功",
		"data": nil,
	})
}

// DisableTaskGroup 禁用任务组
func DisableTaskGroup(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组ID",
			"data": nil,
		})
		return
	}

	// 禁用任务组
	if err := models.UpdateTaskGroupStatus(id, 0); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "禁用任务组失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 重启任务管理器，使变更生效
	taskManager := cron.GetTaskManager()
	if err := taskManager.Restart(); err != nil {
		log.Printf("禁用任务组后重启任务管理器失败: %v", err)
		// 不返回错误，因为任务组状态已经更新成功
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "禁用任务组成功",
		"data": nil,
	})
}

// GetTasksByGroup 获取任务组下的任务
func GetTasksByGroup(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组ID",
			"data": nil,
		})
		return
	}

	// 获取任务组下的任务
	tasks, err := models.GetTasksByGroupID(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务组下的任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": tasks,
	})
}

// GetTaskGroupStats 获取任务组统计信息
func GetTaskGroupStats(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务组ID",
			"data": nil,
		})
		return
	}

	// 获取任务组
	group, err := models.GetTaskGroup(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务组失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 获取任务组下的任务
	tasks, err := models.GetTasksByGroupID(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务组下的任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 统计信息
	totalTasks := len(tasks)
	enabledTasks := 0
	disabledTasks := 0
	successTasks := 0
	failedTasks := 0
	neverRunTasks := 0

	for _, task := range tasks {
		if task.Status == 1 {
			enabledTasks++
		} else {
			disabledTasks++
		}

		if task.LastRunTime.IsZero() {
			neverRunTasks++
		} else if task.LastStatus == 1 {
			successTasks++
		} else {
			failedTasks++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": gin.H{
			"group_id":        group.ID,
			"group_name":      group.Name,
			"group_type":      group.Type,
			"group_status":    group.Status,
			"total_tasks":     totalTasks,
			"enabled_tasks":   enabledTasks,
			"disabled_tasks":  disabledTasks,
			"success_tasks":   successTasks,
			"failed_tasks":    failedTasks,
			"never_run_tasks": neverRunTasks,
		},
	})
}

// GetTaskGroupChart 获取任务组图表数据
func GetTaskGroupChart(c *gin.Context) {
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

	// 获取任务组图表数据
	chart, err := models.GetGroupExecutionChart(id, days)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务组图表数据失败: " + err.Error(),
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
