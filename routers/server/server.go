package server

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"bufio"
	"os"
	"io"
	"time"
)

// ServerLog 获取服务器日志
func ServerLog(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "服务器日志功能待实现",
		"data":   []string{},
	})
}

// Status 获取服务器状态
func Status(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "服务器状态功能待实现",
		"data": map[string]interface{}{
			"running":  false,
			"uptime":   "0",
			"cpu":      "0%",
			"memory":   "0MB",
			"players":  0,
			"maxplayers": 0,
		},
	})
}

// DownloadLog 下载服务器日志
func DownloadLog(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "日志下载功能待实现",
	})
}

// StreamLog 流式传输日志文件
func StreamLog(c *gin.Context) {
	// 获取参数
	archiveName := c.DefaultQuery("archive", "")
	worldName := c.DefaultQuery("world", "")
	
	// 验证参数
	if archiveName == "" || worldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称(archive)和世界名称(world)",
		})
		return
	}
	
	// 构造日志文件路径
	logPath := "/root/DST/Klei/DoNotStarveTogether/" + archiveName + "/" + worldName + "/server_log.txt"
	
	// 检查文件是否存在
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{
			"status": 404,
			"msg":    "日志文件不存在: " + logPath,
		})
		return
	}
	
	// 设置响应头
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	
	// 打开文件
	file, err := os.Open(logPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "打开日志文件失败: " + err.Error(),
		})
		return
	}
	defer file.Close()
	
	// 移动到文件末尾
	file.Seek(0, io.SeekEnd)
	
	// 创建一个reader
	reader := bufio.NewReader(file)
	
	// 创建通道检测客户端连接关闭
	clientGone := c.Request.Context().Done()
	
	c.Stream(func(w io.Writer) bool {
		// 检测连接是否已关闭
		select {
		case <-clientGone:
			return false
		default:
			// 继续处理
		}
		
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 等待新的日志写入
				time.Sleep(500 * time.Millisecond)
				return true
			}
			return false
		}
		
		// 发送数据
		c.SSEvent("log", line)
		return true
	})
} 