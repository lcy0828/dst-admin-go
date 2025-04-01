package server

import (
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"net/http"
	"bufio"
	"os"
	"io"
	"log"
	"time"
	"strings"
	"path/filepath"
)

// 配置变量
var (
	dstSavePath string // DST存档目录
)

// 初始化函数，从配置文件读取配置
func init() {
	// 默认配置
	dstSavePath = "./Klei/DoNotStarveTogether"
	
	// 尝试从配置文件读取
	configFile := "./conf/app.conf"
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
				dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
				log.Printf("从配置文件加载DST存档路径: %s", dstSavePath)
			}
		}
	} else {
		log.Printf("配置文件不存在，使用默认DST存档路径: %s", dstSavePath)
	}
}

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
	lineCount := c.DefaultQuery("lines", "100") // 默认获取最近100行
	
	// 验证参数
	if archiveName == "" || worldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称(archive)和世界名称(world)",
		})
		return
	}
	
	// 构造日志文件路径，使用配置的DST_SAVE_PATH而非硬编码路径
	logPath := filepath.Join(dstSavePath, archiveName, worldName, "server_log.txt")
	
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
	c.Writer.Header().Set("X-Accel-Buffering", "no") // 禁用nginx缓冲
	c.Writer.Flush()
	
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
	
	// 首先读取文件的最后N行
	// 获取文件大小
	stat, err := file.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取文件信息失败: " + err.Error(),
		})
		return
	}
	
	fileSize := stat.Size()
	
	// 创建一个足够大的缓冲区，以容纳文件末尾的部分
	// 我们最多读取文件的最后5MB内容
	bufferSize := int64(5 * 1024 * 1024) // 5MB
	if fileSize < bufferSize {
		bufferSize = fileSize
	}
	
	// 从文件末尾开始往回读取
	offset := fileSize - bufferSize
	if offset < 0 {
		offset = 0
	}
	
	// 设置文件指针位置
	file.Seek(offset, io.SeekStart)
	
	// 读取缓冲区内容
	buffer := make([]byte, bufferSize)
	_, err = file.Read(buffer)
	if err != nil && err != io.EOF {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "读取文件内容失败: " + err.Error(),
		})
		return
	}
	
	// 查找最后N行
	lines := make([]string, 0)
	lastN := 100 // 默认获取最后100行
	
	// 尝试将lines参数转换为整数
	if lineCount == "300" {
		lastN = 300
	}
	
	// 从缓冲区中提取最后N行
	content := string(buffer)
	allLines := strings.Split(content, "\n")
	
	// 如果行数不足，直接使用所有行
	if len(allLines) <= lastN {
		lines = allLines
	} else {
		// 否则，取最后N行
		lines = allLines[len(allLines)-lastN:]
	}
	
	// 发送一个初始事件，以确保连接建立
	c.SSEvent("connected", "true")
	c.Writer.Flush()
	
	// 发送历史日志
	for _, line := range lines {
		if line != "" {
			c.SSEvent("log", line)
			c.Writer.Flush() // 确保每条日志都被立即发送
		}
	}
	
	// 移动到文件末尾准备读取新内容
	file.Seek(0, io.SeekEnd)
	
	// 创建一个reader
	reader := bufio.NewReader(file)
	
	// 创建通道检测客户端连接关闭
	clientGone := c.Request.Context().Done()
	
	// 定期发送心跳以保持连接
	heartbeatTicker := time.NewTicker(15 * time.Second)
	defer heartbeatTicker.Stop()
	
	// 使用channel控制流循环
	logChan := make(chan string)
	errorChan := make(chan error)
	stopChan := make(chan struct{})
	
	// 启动一个goroutine来读取日志
	go func() {
		defer func() {
			close(logChan)
			close(errorChan)
		}()
		
		for {
			select {
			case <-stopChan:
				// 收到停止信号，退出goroutine
				return
			default:
				line, err := reader.ReadString('\n')
				if err != nil {
					if err != io.EOF {
						errorChan <- err
						return
					}
					
					// 如果是EOF，等待新内容
					select {
					case <-stopChan:
						return
					case <-time.After(500 * time.Millisecond):
						continue
					}
				}
				
				// 发送日志行到channel
				select {
				case <-stopChan:
					return
				case logChan <- line:
					// 成功发送
				}
			}
		}
	}()
	
	// 确保在函数返回时发送停止信号
	defer close(stopChan)
	
	// 持续监听新的日志
	c.Stream(func(w io.Writer) bool {
		select {
		case <-clientGone:
			// 客户端断开连接
			return false
			
		case line, ok := <-logChan:
			// 检查channel是否已关闭
			if !ok {
				return false
			}
			// 收到新的日志行
			c.SSEvent("log", line)
			return true
			
		case <-heartbeatTicker.C:
			// 发送心跳
			c.SSEvent("heartbeat", time.Now().String())
			return true
			
		case err, ok := <-errorChan:
			// 检查channel是否已关闭
			if !ok {
				return false
			}
			// 处理错误
			c.SSEvent("error", "读取日志出错: "+err.Error())
			return false
		}
	})
} 