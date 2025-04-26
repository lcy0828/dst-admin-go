package gamelog

import (
	"dont/models"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
)

// CleanupLogRequest 清空日志请求
type CleanupLogRequest struct {
	ArchiveName string `json:"archive_name" binding:"required"` // 存档名称
	WorldName   string `json:"world_name" binding:"required"`   // 世界名称
}

// CleanupLogResponse 清空日志响应
type CleanupLogResponse struct {
	Status int         `json:"status"` // 状态码
	Msg    string      `json:"msg"`    // 消息
	Data   interface{} `json:"data"`   // 数据
}

// CleanupLog 清空数据库中的日志记录并重置日志位置
func CleanupLog(c *gin.Context) {
	var req CleanupLogRequest

	// 解析请求参数
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, CleanupLogResponse{
			Status: 400,
			Msg:    "无效的请求参数: " + err.Error(),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusOK, CleanupLogResponse{
			Status: 400,
			Msg:    "存档名称和世界名称不能为空",
		})
		return
	}

	// 构建日志文件路径
	logFilePath := filepath.Join(dstSavePath, req.ArchiveName, req.WorldName, "server_log.txt")

	// 获取位置管理器
	positionManager := GetPositionManager()

	// 重置日志位置
	positionManager.RemovePosition(logFilePath)

	// 获取日志监控管理器
	key := fmt.Sprintf("%s_%s", req.ArchiveName, req.WorldName)
	logWatchersMutex.Lock()
	watcher, exists := logWatchers[key]
	logWatchersMutex.Unlock()

	// 如果存在监控管理器，重置其状态
	if exists {
		watcher.lastPosition = 0
		watcher.lastModTime = time.Now()
		watcher.processedLines = 0
		log.Printf("[LogCleanup] 重置日志监控管理器状态: %s", key)
	}

	// 清空数据库中的日志记录
	if err := models.CleanupGameLogs(req.ArchiveName, req.WorldName); err != nil {
		log.Printf("[LogCleanup] 清空数据库中的日志记录失败: %v", err)
		c.JSON(http.StatusOK, CleanupLogResponse{
			Status: 500,
			Msg:    "清空数据库中的日志记录失败: " + err.Error(),
		})
		return
	}
	log.Printf("[LogCleanup] 成功清空数据库中的日志记录: %s/%s", req.ArchiveName, req.WorldName)

	// 返回成功响应
	c.JSON(http.StatusOK, CleanupLogResponse{
		Status: 200,
		Msg:    "成功清空数据库中的日志记录并重置日志位置",
		Data: gin.H{
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
			"log_file":     logFilePath,
		},
	})
}
