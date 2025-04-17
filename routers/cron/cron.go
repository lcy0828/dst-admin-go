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

// RegisterCronRoutes 注册定时任务相关路由
func RegisterCronRoutes(router *gin.RouterGroup) {
	cronGroup := router.Group("/cron")
	{
		// 获取所有任务
		cronGroup.GET("/tasks", GetAllTasks)

		// 获取任务详情
		cronGroup.GET("/tasks/:id", GetTaskByID)

		// 添加任务
		cronGroup.POST("/tasks", AddTask)

		// 更新任务
		cronGroup.PUT("/tasks/:id", UpdateTask)

		// 删除任务
		cronGroup.DELETE("/tasks/:id", DeleteTask)

		// 启用任务
		cronGroup.POST("/tasks/:id/enable", EnableTask)

		// 禁用任务
		cronGroup.POST("/tasks/:id/disable", DisableTask)

		// 立即运行任务
		cronGroup.POST("/tasks/:id/run", RunTask)

		// 获取所有内置函数
		cronGroup.GET("/functions", GetAllFunctions)
	}

	// 注册定时任务日志路由
	RegisterCronLogRoutes(router)

	// 注册定时任务组路由
	RegisterCronGroupRoutes(router)

	// 注册定时任务导入导出路由
	RegisterCronExportRoutes(router)

	// 注册定时任务图表路由
	RegisterCronChartRoutes(router)

	// 注册tmux集成路由
	RegisterTmuxIntegrationRoutes(router)

	// 注册tmux任务路由
	RegisterTmuxTaskRoutes(router)
}

// GetAllTasks 获取所有任务
func GetAllTasks(c *gin.Context) {
	tasks, err := models.GetAllTasks()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取任务列表失败: " + err.Error(),
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

// GetTaskByID 获取任务详情
func GetTaskByID(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	task, err := models.GetTaskByID(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "任务不存在: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": task,
	})
}

// TaskRequest 任务请求
type TaskRequest struct {
	Name          string        `json:"name" binding:"required"`
	Description   string        `json:"description"`
	GroupID       int           `json:"group_id"`
	Spec          string        `json:"spec" binding:"required"`
	Type          string        `json:"type" binding:"required"`
	Target        string        `json:"target"` // 不再强制要求
	Args          []interface{} `json:"args"`
	Dependencies  []int         `json:"dependencies"`
	Timeout       int           `json:"timeout"`
	RetryTimes    int           `json:"retry_times"`
	RetryInterval int           `json:"retry_interval"`
	Status        int           `json:"status"`
	// tmux相关字段
	SessionName   string   `json:"session_name"`   // tmux会话名称
	CommandID     string   `json:"command_id"`     // 命令ID（对于tmux_command类型）
	RawCommand    string   `json:"raw_command"`    // 原始命令（对于tmux_raw_command类型）
	CommandParams []string `json:"command_params"` // 命令参数（对于tmux_command类型）
}

// AddTask 添加任务
func AddTask(c *gin.Context) {
	var req TaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 验证任务类型
	validTypes := map[string]bool{
		"function":         true,
		"shell":            true,
		"tmux_command":     true,
		"tmux_raw_command": true,
	}

	if !validTypes[req.Type] {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务类型，支持的类型: function, shell, tmux_command, tmux_raw_command",
			"data": nil,
		})
		return
	}

	// 验证cron表达式
	taskManager := cron.GetTaskManager()
	_, err := taskManager.ValidateSpec(req.Spec)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的cron表达式: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 检查任务组是否存在
	if req.GroupID > 0 {
		_, err := models.GetTaskGroup(req.GroupID)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "任务组不存在: " + err.Error(),
				"data": nil,
			})
			return
		}
	} else {
		// 如果没有指定任务组，使用默认组
		groups, err := models.GetAllTaskGroups()
		if err != nil || len(groups) == 0 {
			c.JSON(http.StatusOK, gin.H{
				"code": e.ERROR,
				"msg":  "获取默认任务组失败",
				"data": nil,
			})
			return
		}
		// 使用第一个任务组作为默认组
		req.GroupID = groups[0].ID
	}

	// 对于tmux相关的任务类型，不需要指定target
	target := req.Target
	if req.Type == "tmux_command" || req.Type == "tmux_raw_command" {
		// 对于tmux相关的任务类型，target可以为空，因为我们根据类型直接调用相应的处理函数
		if target == "" {
			target = "tmux_task" // 设置一个默认值，仅作标记用
		}

		// 检查tmux相关字段
		if req.SessionName == "" {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "tmux会话名称不能为空",
				"data": nil,
			})
			return
		}

		if req.Type == "tmux_command" && req.CommandID == "" {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "tmux命令ID不能为空",
				"data": nil,
			})
			return
		}

		if req.Type == "tmux_raw_command" && req.RawCommand == "" {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "tmux原始命令不能为空",
				"data": nil,
			})
			return
		}
	}

	// 创建任务
	task := &models.CronTask{
		Name:          req.Name,
		Description:   req.Description,
		GroupID:       req.GroupID,
		Spec:          req.Spec,
		Type:          req.Type,
		Target:        target,
		Timeout:       req.Timeout,
		RetryTimes:    req.RetryTimes,
		RetryInterval: req.RetryInterval,
		Status:        req.Status,
		LastStatus:    -1, // 初始状态为未运行
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	// 设置参数
	if err := task.SetArgs(req.Args); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "设置任务参数失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 设置依赖关系
	if len(req.Dependencies) > 0 {
		// 检查依赖任务是否存在
		for _, depID := range req.Dependencies {
			_, err := models.GetTaskByID(depID)
			if err != nil {
				c.JSON(http.StatusOK, gin.H{
					"code": e.INVALID_PARAMS,
					"msg":  "依赖任务不存在 (ID: " + strconv.Itoa(depID) + "): " + err.Error(),
					"data": nil,
				})
				return
			}
		}

		// 设置依赖关系
		if err := task.SetDependencies(req.Dependencies); err != nil {
			c.JSON(http.StatusOK, gin.H{
				"code": e.ERROR,
				"msg":  "设置任务依赖关系失败: " + err.Error(),
				"data": nil,
			})
			return
		}
	}

	// 添加任务
	if err := taskManager.AddTask(task); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "添加任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 如果是tmux相关的任务类型，创建tmux任务详情
	if req.Type == "tmux_command" || req.Type == "tmux_raw_command" {
		tmuxTask := &models.TmuxTask{
			TaskID:      task.ID,
			SessionName: req.SessionName,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}

		if req.Type == "tmux_command" {
			tmuxTask.CommandID = req.CommandID
			// 设置命令参数
			if err := tmuxTask.SetCommandParams(req.CommandParams); err != nil {
				c.JSON(http.StatusOK, gin.H{
					"code": e.ERROR,
					"msg":  "设置命令参数失败: " + err.Error(),
					"data": nil,
				})
				return
			}
		} else {
			tmuxTask.RawCommand = req.RawCommand
		}

		// 创建tmux任务
		if err := models.CreateTmuxTask(tmuxTask); err != nil {
			c.JSON(http.StatusOK, gin.H{
				"code": e.ERROR,
				"msg":  "创建tmux任务失败: " + err.Error(),
				"data": nil,
			})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "添加任务成功",
		"data": task,
	})
}

// UpdateTask 更新任务
func UpdateTask(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	// 获取现有任务
	existingTask, err := models.GetTaskByID(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "任务不存在: " + err.Error(),
			"data": nil,
		})
		return
	}

	var req TaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 验证任务类型
	validTypes := map[string]bool{
		"function":         true,
		"shell":            true,
		"tmux_command":     true,
		"tmux_raw_command": true,
	}

	if !validTypes[req.Type] {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务类型，支持的类型: function, shell, tmux_command, tmux_raw_command",
			"data": nil,
		})
		return
	}

	// 验证cron表达式
	taskManager := cron.GetTaskManager()
	_, err = taskManager.ValidateSpec(req.Spec)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的cron表达式: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 检查任务组是否存在
	if req.GroupID > 0 {
		_, err := models.GetTaskGroup(req.GroupID)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "任务组不存在: " + err.Error(),
				"data": nil,
			})
			return
		}
	}

	// 更新任务
	existingTask.Name = req.Name
	existingTask.Description = req.Description
	if req.GroupID > 0 {
		existingTask.GroupID = req.GroupID
	}
	existingTask.Spec = req.Spec
	existingTask.Type = req.Type

	// 对于tmux相关的任务类型，不需要指定target
	target := req.Target
	if req.Type == "tmux_command" || req.Type == "tmux_raw_command" {
		// 对于tmux相关的任务类型，target可以为空，因为我们根据类型直接调用相应的处理函数
		if target == "" {
			target = "tmux_task" // 设置一个默认值，仅作标记用
		}

		// 检查tmux相关字段
		if req.SessionName == "" {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "tmux会话名称不能为空",
				"data": nil,
			})
			return
		}

		if req.Type == "tmux_command" && req.CommandID == "" {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "tmux命令ID不能为空",
				"data": nil,
			})
			return
		}

		if req.Type == "tmux_raw_command" && req.RawCommand == "" {
			c.JSON(http.StatusOK, gin.H{
				"code": e.INVALID_PARAMS,
				"msg":  "tmux原始命令不能为空",
				"data": nil,
			})
			return
		}
	}
	existingTask.Target = target
	existingTask.Timeout = req.Timeout
	existingTask.RetryTimes = req.RetryTimes
	existingTask.RetryInterval = req.RetryInterval
	existingTask.Status = req.Status
	existingTask.UpdatedAt = time.Now()

	// 设置参数
	if err := existingTask.SetArgs(req.Args); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "设置任务参数失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 设置依赖关系
	if len(req.Dependencies) > 0 {
		// 检查依赖任务是否存在
		for _, depID := range req.Dependencies {
			// 检查是否存在循环依赖
			if depID == existingTask.ID {
				c.JSON(http.StatusOK, gin.H{
					"code": e.INVALID_PARAMS,
					"msg":  "任务不能依赖自身",
					"data": nil,
				})
				return
			}

			_, err := models.GetTaskByID(depID)
			if err != nil {
				c.JSON(http.StatusOK, gin.H{
					"code": e.INVALID_PARAMS,
					"msg":  "依赖任务不存在 (ID: " + strconv.Itoa(depID) + "): " + err.Error(),
					"data": nil,
				})
				return
			}
		}

		// 设置依赖关系
		if err := existingTask.SetDependencies(req.Dependencies); err != nil {
			c.JSON(http.StatusOK, gin.H{
				"code": e.ERROR,
				"msg":  "设置任务依赖关系失败: " + err.Error(),
				"data": nil,
			})
			return
		}
	} else {
		// 清除依赖关系
		if err := existingTask.SetDependencies(nil); err != nil {
			c.JSON(http.StatusOK, gin.H{
				"code": e.ERROR,
				"msg":  "清除任务依赖关系失败: " + err.Error(),
				"data": nil,
			})
			return
		}
	}

	// 更新任务
	if err := taskManager.UpdateTask(existingTask); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "更新任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 如果是tmux相关的任务类型，更新或创建tmux任务详情
	if req.Type == "tmux_command" || req.Type == "tmux_raw_command" {
		// 尝试获取现有的tmux任务
		tmuxTask, err := models.GetTmuxTaskByTaskID(existingTask.ID)
		if err != nil {
			// 如果不存在，创建新的tmux任务
			tmuxTask = &models.TmuxTask{
				TaskID:      existingTask.ID,
				SessionName: req.SessionName,
				CreatedAt:   time.Now(),
				UpdatedAt:   time.Now(),
			}

			if req.Type == "tmux_command" {
				tmuxTask.CommandID = req.CommandID
				// 设置命令参数
				if err := tmuxTask.SetCommandParams(req.CommandParams); err != nil {
					c.JSON(http.StatusOK, gin.H{
						"code": e.ERROR,
						"msg":  "设置命令参数失败: " + err.Error(),
						"data": nil,
					})
					return
				}
			} else {
				tmuxTask.RawCommand = req.RawCommand
			}

			// 创建tmux任务
			if err := models.CreateTmuxTask(tmuxTask); err != nil {
				c.JSON(http.StatusOK, gin.H{
					"code": e.ERROR,
					"msg":  "创建tmux任务失败: " + err.Error(),
					"data": nil,
				})
				return
			}
		} else {
			// 更新现有的tmux任务
			tmuxTask.SessionName = req.SessionName
			tmuxTask.UpdatedAt = time.Now()

			if req.Type == "tmux_command" {
				tmuxTask.CommandID = req.CommandID
				tmuxTask.RawCommand = "" // 清空原始命令
				// 设置命令参数
				if err := tmuxTask.SetCommandParams(req.CommandParams); err != nil {
					c.JSON(http.StatusOK, gin.H{
						"code": e.ERROR,
						"msg":  "设置命令参数失败: " + err.Error(),
						"data": nil,
					})
					return
				}
			} else {
				tmuxTask.RawCommand = req.RawCommand
				tmuxTask.CommandID = ""     // 清空命令ID
				tmuxTask.CommandParams = "" // 清空命令参数
			}

			// 更新tmux任务
			if err := models.UpdateTmuxTask(tmuxTask); err != nil {
				c.JSON(http.StatusOK, gin.H{
					"code": e.ERROR,
					"msg":  "更新tmux任务失败: " + err.Error(),
					"data": nil,
				})
				return
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "更新任务成功",
		"data": existingTask,
	})
}

// DeleteTask 删除任务
func DeleteTask(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	// 先尝试删除tmux任务（如果存在）
	if err := models.DeleteTmuxTask(id); err != nil {
		// 如果删除tmux任务失败，仅记录日志，不中断删除主任务
		log.Printf("删除tmux任务失败 (ID: %d): %v", id, err)
	}

	// 删除任务
	taskManager := cron.GetTaskManager()
	if err := taskManager.DeleteTask(id); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "删除任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "删除任务成功",
		"data": nil,
	})
}

// EnableTask 启用任务
func EnableTask(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	// 启用任务
	taskManager := cron.GetTaskManager()
	if err := taskManager.EnableTask(id); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "启用任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "启用任务成功",
		"data": nil,
	})
}

// DisableTask 禁用任务
func DisableTask(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	// 禁用任务
	taskManager := cron.GetTaskManager()
	if err := taskManager.DisableTask(id); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "禁用任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "禁用任务成功",
		"data": nil,
	})
}

// RunTask 立即运行任务
func RunTask(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的任务ID",
			"data": nil,
		})
		return
	}

	// 运行任务
	taskManager := cron.GetTaskManager()
	logID, err := taskManager.RunTask(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "运行任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "任务已开始运行",
		"data": gin.H{
			"log_id": logID,
		},
	})
}

// GetAllFunctions 获取所有内置函数
func GetAllFunctions(c *gin.Context) {
	taskManager := cron.GetTaskManager()
	functions := taskManager.GetRegisteredFunctions()

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": functions,
	})
}
