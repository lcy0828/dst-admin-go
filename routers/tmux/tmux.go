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
	dstSavePath   string // DST存档目录
	dstUGCPath    string // DST模组目录
	dstServerPath string // DST服务器安装路径
	dstServerMode string // DST服务器启动模式，32或64
)

// 初始化函数，从配置文件读取配置
func init() {
	// 默认配置
	dstSavePath = "./Klei/DoNotStarveTogether"
	dstUGCPath = "./dstserver/ugc_mods"
	dstServerPath = "./dstserver"
	dstServerMode = "64" // 默认使用 64 位模式

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

			if cfg.Section("paths").HasKey("DST_SERVER_PATH") {
				dstServerPath = cfg.Section("paths").Key("DST_SERVER_PATH").String()
				log.Printf("从配置文件加载DST服务器安装路径: %s", dstServerPath)
			}

			if cfg.Section("paths").HasKey("DST_SERVER_MODE") {
				dstServerMode = cfg.Section("paths").Key("DST_SERVER_MODE").String()
				// 验证启动模式是否有效
				if dstServerMode != "32" && dstServerMode != "64" {
					log.Printf("配置文件中的DST服务器启动模式无效: %s，将使用默认值64", dstServerMode)
					dstServerMode = "64"
				} else {
					log.Printf("从配置文件加载DST服务器启动模式: %s", dstServerMode)
				}
			}
		} else {
			log.Printf("加载配置文件失败: %v，将使用默认配置", err)
		}
	} else {
		log.Printf("配置文件不存在，使用默认DST路径配置")
	}

	log.Printf("tmux模块初始化完成，DST存档路径: %s, 模组路径: %s, 服务器安装路径: %s, 启动模式: %s",
		dstSavePath, dstUGCPath, dstServerPath, dstServerMode)
}

// ServerStartRequest 启动服务器请求结构
type ServerStartRequest struct {
	ArchiveName string `json:"archive_name" binding:"required"` // 存档名称
	WorldName   string `json:"world_name" binding:"required"`   // 世界名称
	ServerMode  string `json:"server_mode"`                     // 服务器启动模式，32或64，可选
}

// ServerSessionRequest 会话操作请求结构，用于停止和强制终止服务器
type ServerSessionRequest struct {
	SessionName string `json:"session_name" binding:"required"` // 会话名称
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

	// 如果请求中指定了启动模式，则使用请求中的启动模式
	serverMode := dstServerMode // 默认使用配置文件中的启动模式
	if req.ServerMode != "" {
		// 验证启动模式是否有效
		if req.ServerMode == "32" || req.ServerMode == "64" {
			serverMode = req.ServerMode
			log.Printf("[API][StartServer] 使用请求指定的启动模式: %s", serverMode)
		} else {
			log.Printf("[API][StartServer] 请求指定的启动模式无效: %s，将使用默认值: %s", req.ServerMode, dstServerMode)
		}
	} else {
		log.Printf("[API][StartServer] 未指定启动模式，使用默认模式: %s", serverMode)
	}

	log.Printf("[API][StartServer] 尝试启动服务器 存档: %s, 世界: %s, 启动模式: %s", req.ArchiveName, req.WorldName, serverMode)

	// 创建新的服务器实例
	server, err := tmux.NewDSTServer(
		req.ArchiveName,
		req.WorldName,
		dstUGCPath,
		filepath.Dir(dstSavePath), // 存档根目录是存档路径的父目录
		"DoNotStarveTogether",
		dstServerPath, // 传递服务器安装路径作为启动目录
		serverMode,    // 传递启动模式
	)
	if err != nil {
		log.Printf("[API][StartServer] 创建新的服务器实例失败: %v 存档: %s, 世界: %s", err, req.ArchiveName, req.WorldName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建新的服务器实例失败: " + err.Error(),
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
	log.Printf("[API][StartServer] 服务器启动成功 会话名: %s, 启动模式: %s, 耗时: %v",
		server.SessionName, serverMode, elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "服务器启动成功",
		"data": gin.H{
			"session_name": server.SessionName,
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
			"server_mode":  serverMode,
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
		var req ServerSessionRequest
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

	// 获取服务器实例引用
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		dstUGCPath,
		filepath.Dir(dstSavePath),
		"DoNotStarveTogether",
		dstServerPath, // 传递服务器安装路径作为启动目录
		dstServerMode, // 使用默认启动模式
	)
	if err != nil {
		log.Printf("[API][StopServer] 获取服务器实例引用失败: %v 会话名: %s", err, sessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取服务器实例引用失败: " + err.Error(),
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

// 添加函数，导出配置变量，以便在同一包内的其他文件访问
func GetDSTSavePath() string {
	return dstSavePath
}

func GetDSTUGCPath() string {
	return dstUGCPath
}

func GetDSTServerPath() string {
	return dstServerPath
}

func GetDSTServerMode() string {
	return dstServerMode
}

// SendCommandLegacy 向服务器发送命令处理函数(旧版本，保持向后兼容)
// Deprecated: 使用新的HandleCommand或HandleRawCommand替代
func SendCommandLegacy(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][SendCommandLegacy] 收到发送命令请求 来自IP: %s", clientIP)

	var req ServerCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[API][SendCommandLegacy] 请求参数错误: %v IP: %s", err, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	log.Printf("[API][SendCommandLegacy] 尝试发送命令 会话名: %s, 命令: %s", req.SessionName, req.Command)

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(req.SessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		log.Printf("[API][SendCommandLegacy] 会话名称格式不正确: %s IP: %s", req.SessionName, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "会话名称格式不正确，应为 dstserver_存档名_世界名",
		})
		return
	}

	// 获取服务器实例引用
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		dstUGCPath,
		filepath.Dir(dstSavePath),
		"DoNotStarveTogether",
		dstServerPath, // 传递服务器安装路径作为启动目录
		dstServerMode, // 使用默认启动模式
	)
	if err != nil {
		log.Printf("[API][SendCommandLegacy] 获取服务器实例引用失败: %v 会话名: %s", err, req.SessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取服务器实例引用失败: " + err.Error(),
		})
		return
	}

	// 发送命令
	if err := server.SendCommand(req.Command); err != nil {
		log.Printf("[API][SendCommandLegacy] 发送命令失败: %v 会话名: %s, 命令: %s", err, req.SessionName, req.Command)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "发送命令失败: " + err.Error(),
		})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][SendCommandLegacy] 命令发送成功 会话名: %s, 命令: %s, 耗时: %v", req.SessionName, req.Command, elapsedTime)
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

// SendCommand 是SendCommandLegacy的别名，保持向后兼容
// Deprecated: 使用新的HandleCommand或HandleRawCommand替代
func SendCommand(c *gin.Context) {
	SendCommandLegacy(c)
}

// ListServers 列出所有服务器处理函数
func ListServers(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][ListServers] 收到列出服务器请求 来自IP: %s", clientIP)

	// 获取所有饥荒服务器会话及详细信息
	// 使用silent=false参数，输出正常日志
	serverInfos, err := tmux.ListDSTServers(false)
	if err != nil {
		// 如果错误是因为没有tmux会话，返回空列表而不是错误
		log.Printf("[API][ListServers] 获取服务器列表时发生错误: %v IP: %s", err, clientIP)

		// 检查错误消息是否包含“failed to list sessions”
		if strings.Contains(err.Error(), "failed to list sessions") {
			log.Printf("[API][ListServers] 没有运行中的tmux会话，返回空列表 IP: %s", clientIP)

			// 返回空列表
			serverInfos = []tmux.ServerInfo{}
		} else {
			// 其他错误仍然返回500
			c.JSON(http.StatusInternalServerError, gin.H{
				"status": 500,
				"msg":    "获取服务器列表失败: " + err.Error(),
			})
			return
		}
	}

	log.Printf("[API][ListServers] 获取到 %d 个服务器信息", len(serverInfos))

	elapsedTime := time.Since(startTime)
	log.Printf("[API][ListServers] 获取服务器列表成功，共 %d 个服务器, 耗时: %v", len(serverInfos), elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取服务器列表成功",
		"data":   serverInfos,
		"meta": gin.H{
			"count":        len(serverInfos),
			"elapsed_time": elapsedTime.String(),
		},
	})
}

// RestartServer 重启服务器处理函数
func RestartServer(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][RestartServer] 收到重启服务器请求 来自IP: %s", clientIP)

	sessionName := c.Query("session_name")
	if sessionName == "" {
		var req ServerSessionRequest
		if err := c.ShouldBindJSON(&req); err == nil {
			sessionName = req.SessionName
			log.Printf("[API][RestartServer] 从请求体获取会话名: %s", sessionName)
		} else {
			log.Printf("[API][RestartServer] 请求体解析失败: %v", err)
		}
	} else {
		log.Printf("[API][RestartServer] 从查询参数获取会话名: %s", sessionName)
	}

	if sessionName == "" {
		log.Printf("[API][RestartServer] 未提供会话名称 IP: %s", clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请提供会话名称",
		})
		return
	}

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		log.Printf("[API][RestartServer] 会话名称格式不正确: %s IP: %s", sessionName, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "会话名称格式不正确，应为 dstserver_存档名_世界名",
		})
		return
	}

	log.Printf("[API][RestartServer] 尝试重启服务器 会话名: %s, 存档: %s, 世界: %s",
		sessionName, parts[1], parts[2])

	// 先检查服务器信息是否已经存在，如果存在则使用实际的参数
	serverInfos, _ := tmux.ListDSTServers()
	serverMode := dstServerMode // 默认使用配置文件中的启动模式
	startDir := dstServerPath   // 默认使用配置文件中的启动目录

	// 在服务器信息列表中查找匹配的服务器
	for _, info := range serverInfos {
		if info.SessionName == sessionName {
			// 如果找到匹配的服务器，使用它的实际参数
			if info.ServerMode != "unknown" {
				serverMode = info.ServerMode
				log.Printf("[API][RestartServer] 使用服务器实际的启动模式: %s", serverMode)
			}
			if info.StartDirectory != "" {
				startDir = info.StartDirectory
				log.Printf("[API][RestartServer] 使用服务器实际的启动目录: %s", startDir)
			}
			break
		}
	}

	// 获取服务器实例引用，使用实际的参数
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		dstUGCPath,
		filepath.Dir(dstSavePath),
		"DoNotStarveTogether",
		startDir,   // 使用实际的启动目录
		serverMode, // 使用实际的启动模式
	)
	if err != nil {
		log.Printf("[API][RestartServer] 获取服务器实例引用失败: %v 会话名: %s", err, sessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取服务器实例引用失败: " + err.Error(),
		})
		return
	}

	// 重启服务器
	if err := server.Restart(); err != nil {
		log.Printf("[API][RestartServer] 重启服务器失败: %v 会话名: %s", err, sessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "重启服务器失败: " + err.Error(),
		})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][RestartServer] 服务器已重启 会话名: %s, 模式: %s, 启动目录: %s, 耗时: %v",
		sessionName, serverMode, startDir, elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "服务器已重启",
		"data": gin.H{
			"session_name":    sessionName,
			"archive_name":    parts[1],
			"world_name":      parts[2],
			"server_mode":     serverMode,
			"start_directory": startDir,
			"elapsed_time":    elapsedTime.String(),
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
		var req ServerSessionRequest
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

	// 获取服务器实例引用
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		dstUGCPath,
		filepath.Dir(dstSavePath),
		"DoNotStarveTogether",
		dstServerPath, // 传递服务器安装路径作为启动目录
		dstServerMode, // 使用默认启动模式
	)
	if err != nil {
		log.Printf("[API][KillServer] 获取服务器实例引用失败: %v 会话名: %s", err, sessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取服务器实例引用失败: " + err.Error(),
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
