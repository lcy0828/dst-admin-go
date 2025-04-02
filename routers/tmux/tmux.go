package tmux

import (
	"dont/tmux"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
)

// 配置变量
var (
	dstSavePath string // DST存档目录
	dstUGCPath  string // DST模组目录
)

// 初始化函数，从配置文件读取配置
func init() {
	// 默认配置
	dstSavePath = "./Klei/DoNotStarveTogether"
	dstUGCPath = "./dstserver/ugc_mods"

	// 尝试从配置文件读取
	configFile := "./conf/app.conf"
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
				dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
				log.Printf("从配置文件加载DST存档路径: %s", dstSavePath)
			}

			if cfg.Section("paths").HasKey("DST_UGC_PATH") {
				dstUGCPath = cfg.Section("paths").Key("DST_UGC_PATH").String()
				log.Printf("从配置文件加载DST模组路径: %s", dstUGCPath)
			}
		} else {
			log.Printf("加载配置文件失败: %v，将使用默认配置", err)
		}
	} else {
		log.Printf("配置文件不存在，使用默认DST路径配置")
	}

	log.Printf("tmux模块初始化完成，DST存档路径: %s, 模组路径: %s", dstSavePath, dstUGCPath)
}

// ServerStartRequest 启动服务器请求结构
type ServerStartRequest struct {
	ArchiveName string `json:"archive_name" binding:"required"` // 存档名称
	WorldName   string `json:"world_name" binding:"required"`   // 世界名称
}

// ServerCommandRequest 发送命令请求结构
type ServerCommandRequest struct {
	SessionName string `json:"session_name" binding:"required"` // 会话名称
	Command     string `json:"command" binding:"required"`      // 命令内容
}

// StartServer 启动服务器处理函数
func StartServer(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][StartServer] 收到启动服务器请求 来自IP: %s", clientIP)

	var req ServerStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[API][StartServer] 请求参数错误: %v IP: %s", err, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	log.Printf("[API][StartServer] 尝试启动服务器 存档: %s, 世界: %s", req.ArchiveName, req.WorldName)

	// 创建服务器实例
	server, err := tmux.NewDSTServer(
		req.ArchiveName,
		req.WorldName,
		dstUGCPath,
		filepath.Dir(dstSavePath), // 存档根目录是存档路径的父目录
		"DoNotStarveTogether",
	)
	if err != nil {
		log.Printf("[API][StartServer] 创建服务器实例失败: %v 存档: %s, 世界: %s", err, req.ArchiveName, req.WorldName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建服务器实例失败: " + err.Error(),
		})
		return
	}

	// 启动服务器
	if err := server.Start(); err != nil {
		log.Printf("[API][StartServer] 启动服务器失败: %v 会话名: %s", err, server.SessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "启动服务器失败: " + err.Error(),
		})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][StartServer] 服务器启动成功 会话名: %s, 耗时: %v", server.SessionName, elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "服务器启动成功",
		"data": gin.H{
			"session_name": server.SessionName,
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
			"elapsed_time": elapsedTime.String(),
		},
	})
}

// StopServer 停止服务器处理函数
func StopServer(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][StopServer] 收到停止服务器请求 来自IP: %s", clientIP)

	sessionName := c.Query("session_name")
	if sessionName == "" {
		var req ServerCommandRequest
		if err := c.ShouldBindJSON(&req); err == nil {
			sessionName = req.SessionName
			log.Printf("[API][StopServer] 从请求体获取会话名: %s", sessionName)
		} else {
			log.Printf("[API][StopServer] 请求体解析失败: %v", err)
		}
	} else {
		log.Printf("[API][StopServer] 从查询参数获取会话名: %s", sessionName)
	}

	if sessionName == "" {
		log.Printf("[API][StopServer] 未提供会话名称 IP: %s", clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请提供会话名称",
		})
		return
	}

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		log.Printf("[API][StopServer] 会话名称格式不正确: %s IP: %s", sessionName, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "会话名称格式不正确，应为 dstserver_存档名_世界名",
		})
		return
	}

	log.Printf("[API][StopServer] 尝试停止服务器 会话名: %s, 存档: %s, 世界: %s",
		sessionName, parts[1], parts[2])

	// 创建服务器实例
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		dstUGCPath,
		filepath.Dir(dstSavePath),
		"DoNotStarveTogether",
	)
	if err != nil {
		log.Printf("[API][StopServer] 创建服务器实例失败: %v 会话名: %s", err, sessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建服务器实例失败: " + err.Error(),
		})
		return
	}

	// 停止服务器
	if err := server.Stop(); err != nil {
		log.Printf("[API][StopServer] 停止服务器失败: %v 会话名: %s", err, sessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "停止服务器失败: " + err.Error(),
		})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][StopServer] 已发送停止命令到服务器 会话名: %s, 耗时: %v", sessionName, elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "已发送停止命令到服务器",
		"data": gin.H{
			"session_name": sessionName,
			"elapsed_time": elapsedTime.String(),
		},
	})
}

// SendCommand 向服务器发送命令处理函数
func SendCommand(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][SendCommand] 收到发送命令请求 来自IP: %s", clientIP)

	var req ServerCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[API][SendCommand] 请求参数错误: %v IP: %s", err, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	log.Printf("[API][SendCommand] 尝试发送命令 会话名: %s, 命令: %s", req.SessionName, req.Command)

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(req.SessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		log.Printf("[API][SendCommand] 会话名称格式不正确: %s IP: %s", req.SessionName, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "会话名称格式不正确，应为 dstserver_存档名_世界名",
		})
		return
	}

	// 创建服务器实例
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		dstUGCPath,
		filepath.Dir(dstSavePath),
		"DoNotStarveTogether",
	)
	if err != nil {
		log.Printf("[API][SendCommand] 创建服务器实例失败: %v 会话名: %s", err, req.SessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建服务器实例失败: " + err.Error(),
		})
		return
	}

	// 发送命令
	if err := server.SendCommand(req.Command); err != nil {
		log.Printf("[API][SendCommand] 发送命令失败: %v 会话名: %s, 命令: %s", err, req.SessionName, req.Command)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "发送命令失败: " + err.Error(),
		})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][SendCommand] 命令发送成功 会话名: %s, 命令: %s, 耗时: %v", req.SessionName, req.Command, elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "命令发送成功",
		"data": gin.H{
			"session_name": req.SessionName,
			"command":      req.Command,
			"elapsed_time": elapsedTime.String(),
		},
	})
}

// ListServers 列出所有服务器处理函数
func ListServers(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][ListServers] 收到列出服务器请求 来自IP: %s", clientIP)

	// 获取所有饥荒服务器会话
	sessions, err := tmux.ListDSTServers()
	if err != nil {
		log.Printf("[API][ListServers] 获取服务器列表失败: %v IP: %s", err, clientIP)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取服务器列表失败: " + err.Error(),
		})
		return
	}

	log.Printf("[API][ListServers] 获取到 %d 个服务器会话", len(sessions))

	// 获取每个会话的详细信息
	var serversInfo []map[string]string
	for _, sessionName := range sessions {
		log.Printf("[API][ListServers] 正在获取会话信息: %s", sessionName)
		info, err := tmux.GetSessionInfo(sessionName)
		if err != nil {
			log.Printf("[API][ListServers] 获取会话信息失败: %v, 会话名: %s", err, sessionName)
			continue
		}
		serversInfo = append(serversInfo, info)
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][ListServers] 获取服务器列表成功，共 %d 个服务器, 耗时: %v", len(serversInfo), elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取服务器列表成功",
		"data":   serversInfo,
		"meta": gin.H{
			"count":        len(serversInfo),
			"elapsed_time": elapsedTime.String(),
		},
	})
}

// KillServer 强制终止服务器处理函数
func KillServer(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][KillServer] 收到强制终止服务器请求 来自IP: %s", clientIP)

	sessionName := c.Query("session_name")
	if sessionName == "" {
		var req ServerCommandRequest
		if err := c.ShouldBindJSON(&req); err == nil {
			sessionName = req.SessionName
			log.Printf("[API][KillServer] 从请求体获取会话名: %s", sessionName)
		} else {
			log.Printf("[API][KillServer] 请求体解析失败: %v", err)
		}
	} else {
		log.Printf("[API][KillServer] 从查询参数获取会话名: %s", sessionName)
	}

	if sessionName == "" {
		log.Printf("[API][KillServer] 未提供会话名称 IP: %s", clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请提供会话名称",
		})
		return
	}

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		log.Printf("[API][KillServer] 会话名称格式不正确: %s IP: %s", sessionName, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "会话名称格式不正确，应为 dstserver_存档名_世界名",
		})
		return
	}

	log.Printf("[API][KillServer] 尝试终止服务器 会话名: %s, 存档: %s, 世界: %s",
		sessionName, parts[1], parts[2])

	// 创建服务器实例
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		dstUGCPath,
		filepath.Dir(dstSavePath),
		"DoNotStarveTogether",
	)
	if err != nil {
		log.Printf("[API][KillServer] 创建服务器实例失败: %v 会话名: %s", err, sessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建服务器实例失败: " + err.Error(),
		})
		return
	}

	// 强制终止会话
	if err := server.KillSession(); err != nil {
		log.Printf("[API][KillServer] 终止服务器失败: %v 会话名: %s", err, sessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "终止服务器失败: " + err.Error(),
		})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][KillServer] 服务器已强制终止 会话名: %s, 耗时: %v", sessionName, elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "服务器已强制终止",
		"data": gin.H{
			"session_name": sessionName,
			"elapsed_time": elapsedTime.String(),
		},
	})
}
