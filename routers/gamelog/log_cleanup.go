package gamelog

import (
	"fmt"
	"log"
	"net/http"
	"os"
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

// CleanupLog 清空日志并重置日志位置
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

	// 检查日志文件是否存在
	_, err := os.Stat(logFilePath)
	if os.IsNotExist(err) {
		c.JSON(http.StatusOK, CleanupLogResponse{
			Status: 404,
			Msg:    "日志文件不存在: " + logFilePath,
		})
		return
	}

	// 备份旧日志文件
	backupDir := filepath.Join(dstSavePath, "logs_backup", req.ArchiveName, req.WorldName)
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		log.Printf("[LogCleanup] 创建日志备份目录失败: %v", err)
	} else {
		// 生成备份文件名（使用时间戳）
		timestamp := time.Now().Format("20060102_150405")
		backupFileName := fmt.Sprintf("server_log_%s.txt", timestamp)
		backupFilePath := filepath.Join(backupDir, backupFileName)

		// 复制日志文件
		if err := copyLogFile(logFilePath, backupFilePath); err != nil {
			log.Printf("[LogCleanup] 备份日志文件失败: %v", err)
		} else {
			log.Printf("[LogCleanup] 成功备份日志文件到: %s", backupFilePath)
		}
	}

	// 清空日志文件
	if err := os.WriteFile(logFilePath, []byte(""), 0644); err != nil {
		c.JSON(http.StatusOK, CleanupLogResponse{
			Status: 500,
			Msg:    "清空日志文件失败: " + err.Error(),
		})
		return
	}

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

	// 返回成功响应
	c.JSON(http.StatusOK, CleanupLogResponse{
		Status: 200,
		Msg:    "成功清空日志并重置日志位置",
		Data: gin.H{
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
			"log_file":     logFilePath,
			"backup_dir":   backupDir,
		},
	})
}
