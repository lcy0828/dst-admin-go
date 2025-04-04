package tmux

import (
	"dont/pkg/commands"
	"dont/pkg/commands/types"
	"dont/tmux"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	// 全局命令管理器实例
	commandManager *commands.CommandManager
)

// 初始化函数，创建命令管理器
func init() {
	// 设置命令存储路径
	storagePath := "./conf/commands.json"
	commandManager = commands.NewCommandManager(storagePath)

	// 初始化命令管理器
	if err := commandManager.Initialize(); err != nil {
		log.Printf("[ERROR] 初始化命令管理器失败: %v", err)
	} else {
		log.Printf("[INFO] 命令管理器初始化成功")
	}
}

// CommandRequest 命令请求结构
type CommandRequest struct {
	SessionName string   `json:"session_name" binding:"required"` // 会话名称
	CommandID   string   `json:"command_id" binding:"required"`   // 命令ID
	Params      []string `json:"params,omitempty"`                // 命令参数，可选
}

// RawCommandRequest 原始命令请求结构
type RawCommandRequest struct {
	SessionName string `json:"session_name" binding:"required"` // 会话名称
	Command     string `json:"command" binding:"required"`      // 命令内容
}

// HandleCommand 处理模块化命令请求
func HandleCommand(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][HandleCommand] 收到发送命令请求 来自IP: %s", clientIP)

	var req CommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[API][HandleCommand] 请求参数错误: %v IP: %s", err, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 获取命令
	cmd, err := commandManager.GetCommand(req.CommandID)
	if err != nil {
		log.Printf("[API][HandleCommand] 找不到命令: %s IP: %s", req.CommandID, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "找不到命令: " + req.CommandID,
		})
		return
	}

	// 验证参数
	if !cmd.ValidateParams(req.Params...) {
		log.Printf("[API][HandleCommand] 命令参数无效: %v IP: %s", req.Params, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "命令参数无效",
		})
		return
	}

	// 生成脚本
	script := cmd.GenerateScript(req.Params...)
	log.Printf("[API][HandleCommand] 尝试发送命令 会话名: %s, 命令ID: %s, 脚本: %s",
		req.SessionName, req.CommandID, script)

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(req.SessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		log.Printf("[API][HandleCommand] 会话名称格式不正确: %s IP: %s", req.SessionName, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "会话名称格式不正确，应为 dstserver_存档名_世界名",
		})
		return
	}

	// 从tmux.go获取配置变量
	savePath := GetDSTSavePath()
	ugcPath := GetDSTUGCPath()
	serverPath := GetDSTServerPath()
	serverMode := GetDSTServerMode()

	// 获取服务器实例引用
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		ugcPath,
		filepath.Dir(savePath),
		"DoNotStarveTogether",
		serverPath, // 传递服务器安装路径作为启动目录
		serverMode, // 使用默认启动模式
	)
	if err != nil {
		log.Printf("[API][HandleCommand] 获取服务器实例引用失败: %v 会话名: %s", err, req.SessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取服务器实例引用失败: " + err.Error(),
		})
		return
	}

	// 发送命令
	if err := server.SendCommand(script); err != nil {
		log.Printf("[API][HandleCommand] 发送命令失败: %v 会话名: %s, 命令: %s", err, req.SessionName, script)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "发送命令失败: " + err.Error(),
		})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][HandleCommand] 命令发送成功 会话名: %s, 命令ID: %s, 耗时: %v",
		req.SessionName, req.CommandID, elapsedTime)
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "命令发送成功",
		"data": gin.H{
			"session_name": req.SessionName,
			"command_id":   req.CommandID,
			"command_name": cmd.Name,
			"script":       script,
			"elapsed_time": elapsedTime.String(),
		},
	})
}

// HandleRawCommand 处理原始命令请求
func HandleRawCommand(c *gin.Context) {
	startTime := time.Now()
	clientIP := c.ClientIP()
	log.Printf("[API][HandleRawCommand] 收到发送原始命令请求 来自IP: %s", clientIP)

	var req RawCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[API][HandleRawCommand] 请求参数错误: %v IP: %s", err, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	log.Printf("[API][HandleRawCommand] 尝试发送原始命令 会话名: %s, 命令: %s", req.SessionName, req.Command)

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(req.SessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		log.Printf("[API][HandleRawCommand] 会话名称格式不正确: %s IP: %s", req.SessionName, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "会话名称格式不正确，应为 dstserver_存档名_世界名",
		})
		return
	}

	// 从tmux.go获取配置变量
	savePath := GetDSTSavePath()
	ugcPath := GetDSTUGCPath()
	serverPath := GetDSTServerPath()
	serverMode := GetDSTServerMode()

	// 获取服务器实例引用
	server, err := tmux.NewDSTServer(
		parts[1],
		parts[2],
		ugcPath,
		filepath.Dir(savePath),
		"DoNotStarveTogether",
		serverPath, // 传递服务器安装路径作为启动目录
		serverMode, // 使用默认启动模式
	)
	if err != nil {
		log.Printf("[API][HandleRawCommand] 获取服务器实例引用失败: %v 会话名: %s", err, req.SessionName)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取服务器实例引用失败: " + err.Error(),
		})
		return
	}

	// 发送命令
	if err := server.SendCommand(req.Command); err != nil {
		log.Printf("[API][HandleRawCommand] 发送命令失败: %v 会话名: %s, 命令: %s", err, req.SessionName, req.Command)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "发送命令失败: " + err.Error(),
		})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("[API][HandleRawCommand] 命令发送成功 会话名: %s, 命令: %s, 耗时: %v",
		req.SessionName, req.Command, elapsedTime)
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

// ListCommands 获取所有命令
func ListCommands(c *gin.Context) {
	category := c.Query("category")

	var commands []*types.Command
	if category != "" {
		commands = commandManager.GetCommandsByCategory(category)
	} else {
		commands = commandManager.GetAllCommands()
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取命令列表成功",
		"data":   commands,
	})
}

// GetCommandDetail 获取单个命令详情
func GetCommandDetail(c *gin.Context) {
	var req struct {
		ID string `json:"id" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	cmd, err := commandManager.GetCommand(req.ID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"status": 404,
			"msg":    "命令不存在",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取命令成功",
		"data":   cmd,
	})
}

// AddCommand 添加新命令
func AddCommand(c *gin.Context) {
	var cmd types.Command
	if err := c.ShouldBindJSON(&cmd); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	if err := commandManager.AddCommand(&cmd); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "添加命令失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "添加命令成功",
		"data":   cmd,
	})
}

// UpdateCommand 更新命令
func UpdateCommand(c *gin.Context) {
	var cmd types.Command
	if err := c.ShouldBindJSON(&cmd); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 确保ID不为空
	if cmd.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "命令ID不能为空",
		})
		return
	}

	if err := commandManager.UpdateCommand(&cmd); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "更新命令失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新命令成功",
		"data":   cmd,
	})
}

// DeleteCommand 删除命令
func DeleteCommand(c *gin.Context) {
	var req struct {
		ID string `json:"id" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	if err := commandManager.DeleteCommand(req.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "删除命令失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "删除命令成功",
	})
}
