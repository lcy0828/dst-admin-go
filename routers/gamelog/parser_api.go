package gamelog

import (
	"dont/models"
	"dont/service/logparser"
	"dont/tmux"
	"log"
	"net/http"
	"os"
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
	router.GET("/parser/startup_versions", GetStartupVersions)    // 添加获取启动版本列表的API
	router.GET("/parser/archives_with_logs", GetArchivesWithLogs) // 添加获取有日志的存档和世界列表的API
	router.POST("/parser/cleanup_log", CleanupLog)                // 添加清空日志并重置日志位置的API
}

// GetParsedLogs 获取解析后的日志数据
func GetParsedLogs(c *gin.Context) {
	// 打印调试信息
	log.Printf("[GameLog] GetParsedLogs: 开始获取解析后的日志数据")

	// 解析请求参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")
	logType := c.Query("type")
	startupVersion := c.Query("startup_version") // 添加启动版本参数
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))

	log.Printf("[GameLog] GetParsedLogs: 请求参数: 存档=%s, 世界=%s, 类型=%s, 启动版本=%s, 页码=%d, 每页数量=%d",
		archiveName, worldName, logType, startupVersion, page, pageSize)

	// 解析时间范围
	startTimeStr := c.Query("start_time")
	endTimeStr := c.Query("end_time")

	var startTime, endTime time.Time
	var err error

	if startTimeStr != "" {
		startTime, err = time.Parse(time.RFC3339, startTimeStr)
		if err != nil {
			log.Printf("[GameLog] GetParsedLogs: 开始时间格式错误: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{
				"status": 400,
				"msg":    "开始时间格式错误: " + err.Error(),
			})
			return
		}
		log.Printf("[GameLog] GetParsedLogs: 开始时间=%s", startTime.Format(time.RFC3339))
	}

	if endTimeStr != "" {
		endTime, err = time.Parse(time.RFC3339, endTimeStr)
		if err != nil {
			log.Printf("[GameLog] GetParsedLogs: 结束时间格式错误: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{
				"status": 400,
				"msg":    "结束时间格式错误: " + err.Error(),
			})
			return
		}
		log.Printf("[GameLog] GetParsedLogs: 结束时间=%s", endTime.Format(time.RFC3339))
	} else {
		endTime = time.Now()
		log.Printf("[GameLog] GetParsedLogs: 未指定结束时间，使用当前时间=%s", endTime.Format(time.RFC3339))
	}

	log.Printf("[GameLog] GetParsedLogs: 开始查询数据库")

	// 获取日志数据
	logs, total, err := models.GetGameLogs(archiveName, worldName, logType, startupVersion, startTime, endTime, page, pageSize)
	if err != nil {
		log.Printf("[GameLog] GetParsedLogs: 获取日志数据失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取日志数据失败: " + err.Error(),
		})
		return
	}

	log.Printf("[GameLog] GetParsedLogs: 查询成功，返回 %d 条记录，总数 %d", len(logs), total)

	// 如果没有日志数据，尝试手动解析日志文件
	if len(logs) == 0 && total == 0 {
		log.Printf("[GameLog] GetParsedLogs: 没有日志数据，尝试手动解析日志文件")

		// 获取服务器列表
		// 使用silent=false参数，输出正常日志
		servers := tmux.GetRunningServers(false)
		log.Printf("[GameLog] GetParsedLogs: 获取到 %d 个运行中的服务器", len(servers))

		// 手动解析日志文件
		for _, server := range servers {
			// 构建日志文件路径
			logFilePath := "/Users/lcy/DoNotStarveTogether/" + server.ArchiveName + "/" + server.WorldName + "/server_log.txt"
			log.Printf("[GameLog] GetParsedLogs: 尝试解析日志文件: %s", logFilePath)

			// 检查文件是否存在
			if _, err := os.Stat(logFilePath); os.IsNotExist(err) {
				log.Printf("[GameLog] GetParsedLogs: 日志文件不存在: %s", logFilePath)
				continue
			}

			// 读取日志文件内容
			logContent, err := os.ReadFile(logFilePath)
			if err != nil {
				log.Printf("[GameLog] GetParsedLogs: 读取日志文件失败: %v", err)
				continue
			}

			log.Printf("[GameLog] GetParsedLogs: 成功读取日志文件，大小: %d 字节", len(logContent))

			// 创建日志解析器
			parser, err := logparser.NewLogParser(server.ArchiveName, server.WorldName)
			if err != nil {
				log.Printf("[GameLog] GetParsedLogs: 创建日志解析器失败: %v", err)
				continue
			}

			// 解析日志内容
			if err := parser.ProcessAndSaveLog(string(logContent)); err != nil {
				log.Printf("[GameLog] GetParsedLogs: 解析日志内容失败: %v", err)
			} else {
				log.Printf("[GameLog] GetParsedLogs: 成功解析日志内容")
			}
		}

		// 重新查询数据库
		logs, total, err = models.GetGameLogs(archiveName, worldName, logType, startupVersion, startTime, endTime, page, pageSize)
		if err != nil {
			log.Printf("[GameLog] GetParsedLogs: 重新查询数据库失败: %v", err)
		} else {
			log.Printf("[GameLog] GetParsedLogs: 重新查询数据库成功，返回 %d 条记录，总数 %d", len(logs), total)
		}
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

	log.Printf("[GameLog] GetParsedLogs: 已返回响应")
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
		MatchMode   string `json:"match_mode"`   // 匹配模式：single(单行), multi_line(多行), head_tail(首尾行), fixed_lines(固定行数)
		TailPattern string `json:"tail_pattern"` // 尾行匹配模式（仅当match_mode为head_tail时有效）
		LineCount   int    `json:"line_count"`   // 固定行数（仅当match_mode为fixed_lines时有效）
	}

	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 如果未指定匹配模式，默认为单行匹配
	if rule.MatchMode == "" {
		rule.MatchMode = models.MatchModeSingle
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
		rule.MatchMode,
		rule.TailPattern,
		rule.LineCount,
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
		MatchMode   string `json:"match_mode"`   // 匹配模式：single(单行), multi_line(多行), head_tail(首尾行), fixed_lines(固定行数)
		TailPattern string `json:"tail_pattern"` // 尾行匹配模式（仅当match_mode为head_tail时有效）
		LineCount   int    `json:"line_count"`   // 固定行数（仅当match_mode为fixed_lines时有效）
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
		rule.MatchMode,
		rule.TailPattern,
		rule.LineCount,
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

// GetStartupVersions 获取启动版本列表
func GetStartupVersions(c *gin.Context) {
	// 解析请求参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")

	log.Printf("[GameLog] GetStartupVersions: 请求参数: 存档=%s, 世界=%s",
		archiveName, worldName)

	// 获取启动版本列表
	manager := logparser.GetLogParserManager()
	versions, err := manager.GetStartupVersions(archiveName, worldName)
	if err != nil {
		log.Printf("[GameLog] GetStartupVersions: 获取启动版本列表失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取启动版本列表失败: " + err.Error(),
		})
		return
	}

	log.Printf("[GameLog] GetStartupVersions: 查询成功，返回 %d 个启动版本", len(versions))

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取启动版本列表成功",
		"data":   versions,
	})
}

// GetArchivesWithLogs 获取有日志的存档和世界列表
func GetArchivesWithLogs(c *gin.Context) {
	log.Printf("[GameLog] GetArchivesWithLogs: 开始获取有日志的存档和世界列表")

	// 获取有日志的存档和世界列表
	manager := logparser.GetLogParserManager()
	archives, err := manager.GetArchivesWithLogs()
	if err != nil {
		log.Printf("[GameLog] GetArchivesWithLogs: 获取有日志的存档和世界列表失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取有日志的存档和世界列表失败: " + err.Error(),
		})
		return
	}

	log.Printf("[GameLog] GetArchivesWithLogs: 查询成功，返回 %d 个存档信息", len(archives))

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取有日志的存档和世界列表成功",
		"data":   archives,
	})
}
