package parser

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"dont/service/logmonitor"
)

// ParserStatus 解析器状态结构体
type ParserStatus struct {
	ID             string    `json:"id"`              // 解析器ID
	ArchiveName    string    `json:"archive_name"`    // 存档名称
	WorldName      string    `json:"world_name"`      // 世界名称
	ServerType     string    `json:"server_type"`     // 服务器类型（forest或cave）
	StartTime      time.Time `json:"start_time"`      // 启动时间
	LogFile        string    `json:"log_file"`        // 日志文件路径
	Status         string    `json:"status"`          // 状态（running或stopped）
	ProcessedLines int64     `json:"processed_lines"` // 已处理行数
	LastActivity   time.Time `json:"last_activity"`   // 最后活动时间
	ClientCount    int       `json:"client_count"`    // 当前连接的客户端数量
}

// GetActiveParsers 获取当前运行中的解析器
func GetActiveParsers(c *gin.Context) {
	// 打印调试信息
	log.Printf("[Parser] GetActiveParsers: 开始获取活跃解析器")

	// 获取动态日志监控服务实例
	monitor := logmonitor.GetDynamicLogMonitor()
	if monitor == nil {
		log.Printf("[Parser] GetActiveParsers: 动态日志监控服务未启动")
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "动态日志监控服务未启动",
		})
		return
	}

	log.Printf("[Parser] GetActiveParsers: 成功获取动态日志监控服务实例")

	// 获取所有活跃的监控器
	watchers := monitor.GetAllWatchers()
	log.Printf("[Parser] GetActiveParsers: 获取到 %d 个监控器", len(watchers))

	// 构建响应数据
	var parsers []ParserStatus
	for key, watcher := range watchers {
		// 获取解析器状态
		status := ParserStatus{
			ID:             key,
			ArchiveName:    watcher.GetArchiveName(),
			WorldName:      watcher.GetWorldName(),
			ServerType:     watcher.GetServerType(),
			StartTime:      watcher.GetStartTime(),
			LogFile:        watcher.GetLogFile(),
			Status:         "running",
			ProcessedLines: watcher.GetProcessedLines(),
			LastActivity:   watcher.GetLastActivity(),
			ClientCount:    watcher.GetClientCount(),
		}

		parsers = append(parsers, status)
	}

	// 确保返回空数组而不是null
	if parsers == nil {
		parsers = []ParserStatus{}
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取运行中的解析器成功",
		"data":   parsers,
	})
}
