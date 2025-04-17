package cron

import (
	"dont/models"
	"dont/pkg/e"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// RegisterTmuxTaskRoutes 注册tmux任务相关路由
func RegisterTmuxTaskRoutes(router *gin.RouterGroup) {
	tmuxTaskGroup := router.Group("/cron/tmux-tasks")
	{
		// 获取tmux任务详情
		tmuxTaskGroup.GET("/:task_id", GetTmuxTaskByTaskID)
	}
}

// GetTmuxTaskByTaskID 根据任务ID获取tmux任务详情
func GetTmuxTaskByTaskID(c *gin.Context) {
	taskIDStr := c.Param("task_id")
	taskID, err := strconv.Atoi(taskIDStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	// 获取tmux任务详情
	tmuxTask, err := models.GetTmuxTaskByTaskID(taskID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取tmux任务详情失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 获取命令参数
	params, err := tmuxTask.GetCommandParams()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "解析命令参数失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 构建响应数据
	data := map[string]interface{}{
		"id":             tmuxTask.ID,
		"task_id":        tmuxTask.TaskID,
		"session_name":   tmuxTask.SessionName,
		"command_id":     tmuxTask.CommandID,
		"raw_command":    tmuxTask.RawCommand,
		"command_params": params,
		"created_at":     tmuxTask.CreatedAt,
		"updated_at":     tmuxTask.UpdatedAt,
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": data,
	})
}
