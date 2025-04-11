package gamelog

import (
	"dont/models"
	"dont/service/logparser"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// 注册日志解析API路由
func RegisterParserAPIRoutes(router *gin.RouterGroup) {
	router.GET("/parser/logs", GetParsedLogs)
	router.GET("/parser/log_types", GetLogTypes)
	router.GET("/parser/log_statistics", GetLogStatistics)
	router.GET("/parser/rules", GetExtractRules)
	router.POST("/parser/rules", AddExtractRule)
	router.PUT("/parser/rules/:id", UpdateExtractRule)
	router.DELETE("/parser/rules/:id", DeleteExtractRule)
	router.GET("/parser/search", SearchParsedLogs)
	router.POST("/parser/toggle", ToggleLogParser)
}

// GetParsedLogs 获取解析后的日志数据
func GetParsedLogs(c *gin.Context) {
	// 解析请求参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")
	logType := c.Query("type")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))

	// 解析时间范围
	startTimeStr := c.Query("start_time")
	endTimeStr := c.Query("end_time")

	var startTime, endTime time.Time
	var err error

	if startTimeStr != "" {
		startTime, err = time.Parse(time.RFC3339, startTimeStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"status": 400,
				"msg":    "开始时间格式错误: " + err.Error(),
			})
			return
		}
	}

	if endTimeStr != "" {
		endTime, err = time.Parse(time.RFC3339, endTimeStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"status": 400,
				"msg":    "结束时间格式错误: " + err.Error(),
			})
			return
		}
	} else {
		endTime = time.Now()
	}

	// 获取日志数据
	logs, total, err := models.GetGameLogs(archiveName, worldName, logType, startTime, endTime, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取日志数据失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取日志数据成功",
		"data": gin.H{
			"logs":  logs,
			"total": total,
			"page":  page,
			"size":  pageSize,
		},
	})
}

// GetLogTypes 获取日志类型统计
func GetLogTypes(c *gin.Context) {
	// 解析请求参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")

	// 获取日志类型统计
	manager := logparser.GetLogParserManager()
	counts, err := manager.GetLogTypeDistribution(archiveName, worldName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取日志类型统计失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取日志类型统计成功",
		"data":   counts,
	})
}

// GetLogStatistics 获取日志统计信息
func GetLogStatistics(c *gin.Context) {
	// 解析请求参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))

	// 获取日志统计信息
	manager := logparser.GetLogParserManager()
	stats, err := manager.GetLogStatistics(archiveName, worldName, days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取日志统计信息失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取日志统计信息成功",
		"data":   stats,
	})
}

// GetExtractRules 获取所有日志提取规则
func GetExtractRules(c *gin.Context) {
	// 获取所有规则
	manager := logparser.GetLogParserManager()
	rules, err := manager.GetAllRules()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取规则列表失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取规则列表成功",
		"data":   rules,
	})
}

// AddExtractRule 添加日志提取规则
func AddExtractRule(c *gin.Context) {
	// 解析请求参数
	var rule struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		LogType     string `json:"log_type" binding:"required"`
		Pattern     string `json:"pattern" binding:"required"`
		IsRegex     bool   `json:"is_regex"`
		IsEnabled   bool   `json:"is_enabled"`
		Priority    int    `json:"priority"`
	}

	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 添加规则
	manager := logparser.GetLogParserManager()
	err := manager.AddCustomRule(
		rule.Name,
		rule.Description,
		rule.LogType,
		rule.Pattern,
		rule.IsRegex,
		rule.IsEnabled,
		rule.Priority,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "添加规则失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "添加规则成功",
	})
}

// UpdateExtractRule 更新日志提取规则
func UpdateExtractRule(c *gin.Context) {
	// 解析规则ID
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "无效的规则ID",
		})
		return
	}

	// 解析请求参数
	var rule struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		LogType     string `json:"log_type" binding:"required"`
		Pattern     string `json:"pattern" binding:"required"`
		IsRegex     bool   `json:"is_regex"`
		IsEnabled   bool   `json:"is_enabled"`
		Priority    int    `json:"priority"`
	}

	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 更新规则
	manager := logparser.GetLogParserManager()
	err = manager.UpdateCustomRule(
		id,
		rule.Name,
		rule.Description,
		rule.LogType,
		rule.Pattern,
		rule.IsRegex,
		rule.IsEnabled,
		rule.Priority,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "更新规则失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新规则成功",
	})
}

// DeleteExtractRule 删除日志提取规则
func DeleteExtractRule(c *gin.Context) {
	// 解析规则ID
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "无效的规则ID",
		})
		return
	}

	// 删除规则
	manager := logparser.GetLogParserManager()
	err = manager.DeleteCustomRule(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "删除规则失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "删除规则成功",
	})
}

// SearchParsedLogs 搜索日志
func SearchParsedLogs(c *gin.Context) {
	// 解析请求参数
	keyword := c.Query("keyword")
	if keyword == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "缺少搜索关键词",
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))

	// 搜索日志
	manager := logparser.GetLogParserManager()
	logs, total, err := manager.SearchLogs(keyword, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "搜索日志失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "搜索日志成功",
		"data": gin.H{
			"logs":  logs,
			"total": total,
			"page":  page,
			"size":  pageSize,
		},
	})
}

// ToggleLogParser 切换日志解析器状态
func ToggleLogParser(c *gin.Context) {
	// 解析请求参数
	var req struct {
		ArchiveName string `json:"archive_name" binding:"required"`
		WorldName   string `json:"world_name" binding:"required"`
		Enabled     bool   `json:"enabled"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 获取日志监控器
	watcher, err := getOrCreateLogWatcher(req.ArchiveName, req.WorldName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取日志监控器失败: " + err.Error(),
		})
		return
	}

	// 切换状态
	watcher.enableDBStore = req.Enabled

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg": func() string {
			if req.Enabled {
				return "日志解析器已启用"
			} else {
				return "日志解析器已禁用"
			}
		}(),
		"data": gin.H{
			"enabled": req.Enabled,
		},
	})
}
