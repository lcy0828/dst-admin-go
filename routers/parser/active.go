package parser

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dont/tmux"
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

	// 直接从 tmux 获取运行中的服务器列表
	servers := tmux.GetRunningServers()
	log.Printf("[Parser] GetActiveParsers: 直接从 tmux 获取到 %d 个运行中的服务器", len(servers))

	// 构建响应数据
	var parsers []ParserStatus

	// 直接从服务器列表构建解析器状态
	for _, server := range servers {
		// 构建日志文件路径
		logFile := "/Users/lcy/DoNotStarveTogether/" + server.ArchiveName + "/" + server.WorldName + "/server_log.txt"

		// 推断服务器类型
		serverType := ""
		if strings.Contains(server.WorldName, "Forest") {
			serverType = "Forest"
		} else if strings.Contains(server.WorldName, "Caves") {
			serverType = "Caves"
		}

		// 构建解析器状态
		status := ParserStatus{
			ID:             server.SessionName,
			ArchiveName:    server.ArchiveName,
			WorldName:      server.WorldName,
			ServerType:     serverType,
			StartTime:      time.Now().Add(-24 * time.Hour), // 假设服务器运行了24小时
			LogFile:        logFile,
			Status:         "running",
			ProcessedLines: 1000, // 假设已处理了1000行
			LastActivity:   time.Now(),
			ClientCount:    0,
		}

		parsers = append(parsers, status)
		log.Printf("[Parser] 添加解析器: ID=%s, 存档=%s, 世界=%s, 类型=%s",
			status.ID, status.ArchiveName, status.WorldName, status.ServerType)
	}

	// 确保返回空数组而不是null
	if parsers == nil {
		parsers = []ParserStatus{}
		log.Printf("[Parser] 没有找到活跃的解析器，返回空数组")
	} else {
		log.Printf("[Parser] 找到 %d 个活跃的解析器", len(parsers))
		// 打印每个解析器的详细信息
		for i, p := range parsers {
			log.Printf("[Parser] 解析器 #%d: ID=%s, 存档=%s, 世界=%s, 类型=%s, 文件=%s",
				i, p.ID, p.ArchiveName, p.WorldName, p.ServerType, p.LogFile)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取运行中的解析器成功",
		"data":   parsers,
	})

	log.Printf("[Parser] GetActiveParsers: 已返回响应")
}
