package tmux

import (
	"dont/tmux"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
		}
	} else {
		log.Printf("配置文件不存在，使用默认DST路径配置")
	}
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
	var req ServerStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 创建服务器实例
	server, err := tmux.NewDSTServer(
		req.ArchiveName,
		req.WorldName,
		dstUGCPath,
		filepath.Dir(dstSavePath), // 存档根目录是存档路径的父目录
		"DoNotStarveTogether",
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建服务器实例失败: " + err.Error(),
		})
		return
	}

	// 启动服务器
	if err := server.Start(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "启动服务器失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "服务器启动成功",
		"data": gin.H{
			"session_name": server.SessionName,
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
		},
	})
}

// StopServer 停止服务器处理函数
func StopServer(c *gin.Context) {
	sessionName := c.Query("session_name")
	if sessionName == "" {
		var req ServerCommandRequest
		if err := c.ShouldBindJSON(&req); err == nil {
			sessionName = req.SessionName
		}
	}

	if sessionName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请提供会话名称",
		})
		return
	}

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建服务器实例失败: " + err.Error(),
		})
		return
	}

	// 停止服务器
	if err := server.Stop(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "停止服务器失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "已发送停止命令到服务器",
		"data": gin.H{
			"session_name": sessionName,
		},
	})
}

// SendCommand 向服务器发送命令处理函数
func SendCommand(c *gin.Context) {
	var req ServerCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(req.SessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建服务器实例失败: " + err.Error(),
		})
		return
	}

	// 发送命令
	if err := server.SendCommand(req.Command); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "发送命令失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "命令发送成功",
		"data": gin.H{
			"session_name": req.SessionName,
			"command":      req.Command,
		},
	})
}

// ListServers 列出所有服务器处理函数
func ListServers(c *gin.Context) {
	// 获取所有饥荒服务器会话
	sessions, err := tmux.ListDSTServers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取服务器列表失败: " + err.Error(),
		})
		return
	}

	// 获取每个会话的详细信息
	var serversInfo []map[string]string
	for _, sessionName := range sessions {
		info, err := tmux.GetSessionInfo(sessionName)
		if err != nil {
			log.Printf("获取会话信息失败: %v", err)
			continue
		}
		serversInfo = append(serversInfo, info)
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取服务器列表成功",
		"data":   serversInfo,
	})
}

// KillServer 强制终止服务器处理函数
func KillServer(c *gin.Context) {
	sessionName := c.Query("session_name")
	if sessionName == "" {
		var req ServerCommandRequest
		if err := c.ShouldBindJSON(&req); err == nil {
			sessionName = req.SessionName
		}
	}

	if sessionName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请提供会话名称",
		})
		return
	}

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建服务器实例失败: " + err.Error(),
		})
		return
	}

	// 强制终止会话
	if err := server.KillSession(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "终止服务器失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "服务器已强制终止",
		"data": gin.H{
			"session_name": sessionName,
		},
	})
}
