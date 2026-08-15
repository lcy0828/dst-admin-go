package cron

import (
	"context"
	"dont/pkg/commands"
	"dont/pkg/e"
	"dont/pkg/taskbridge"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type LegacyRuntimeSession struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type LegacyRuntimeCatalog interface {
	Sessions(context.Context) ([]LegacyRuntimeSession, error)
}

var legacyRuntimeCatalog LegacyRuntimeCatalog
var legacyRuntimeConsole taskbridge.RuntimeConsole

func ConfigureLegacyRuntime(catalog LegacyRuntimeCatalog, console taskbridge.RuntimeConsole) {
	legacyRuntimeCatalog, legacyRuntimeConsole = catalog, console
}

// RegisterTmuxIntegrationRoutes 注册tmux集成相关路由
func RegisterTmuxIntegrationRoutes(router *gin.RouterGroup) {
	tmuxGroup := router.Group("/cron/tmux")
	{
		// 获取可用的tmux会话列表
		tmuxGroup.GET("/sessions", GetTmuxSessions)

		// 获取可用的命令列表
		tmuxGroup.GET("/commands", GetTmuxCommands)

		// 测试tmux命令
		tmuxGroup.POST("/test-command", TestTmuxCommand)

		// 测试tmux原始命令
		tmuxGroup.POST("/test-raw-command", TestTmuxRawCommand)
	}
}

// GetTmuxSessions 获取可用的tmux会话列表
func GetTmuxSessions(c *gin.Context) {
	if legacyRuntimeCatalog == nil {
		c.JSON(http.StatusOK, gin.H{"code": e.ERROR, "msg": "旧 tmux 清单已停用，请使用 Runtime 目标和房间拓扑", "data": nil})
		return
	}
	sessions, err := legacyRuntimeCatalog.Sessions(c.Request.Context())
	if err != nil {
		log.Printf("[API][GetTmuxSessions] 获取tmux会话列表失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取tmux会话列表失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": sessions,
	})
}

// GetTmuxCommands 获取可用的命令列表
func GetTmuxCommands(c *gin.Context) {
	category := c.Query("category")

	// 获取命令管理器
	cmdManager := commands.CreateCommandManager("./conf/commands.json")
	if err := cmdManager.Initialize(); err != nil {
		log.Printf("[API][GetTmuxCommands] 初始化命令管理器失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "初始化命令管理器失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 获取命令列表
	var cmds []*commands.Command
	if category != "" {
		cmds = cmdManager.GetCommandsByCategory(category)
	} else {
		cmds = cmdManager.GetAllCommands()
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": cmds,
	})
}

// TmuxCommandTestRequest 测试tmux命令请求
type TmuxCommandTestRequest struct {
	SessionName string   `json:"session_name" binding:"required"` // 会话名称
	CommandID   string   `json:"command_id" binding:"required"`   // 命令ID
	Params      []string `json:"params,omitempty"`                // 命令参数，可选
}

// TestTmuxCommand 测试tmux命令
func TestTmuxCommand(c *gin.Context) {
	var req TmuxCommandTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 获取命令管理器
	cmdManager := commands.CreateCommandManager("./conf/commands.json")
	if err := cmdManager.Initialize(); err != nil {
		log.Printf("[API][TestTmuxCommand] 初始化命令管理器失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "初始化命令管理器失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 获取命令
	cmd, err := cmdManager.GetCommand(req.CommandID)
	if err != nil {
		log.Printf("[API][TestTmuxCommand] 找不到命令: %s", req.CommandID)
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "找不到命令: " + req.CommandID,
			"data": nil,
		})
		return
	}

	// 验证参数
	if !cmd.ValidateParams(req.Params...) {
		log.Printf("[API][TestTmuxCommand] 命令参数无效: %v", req.Params)
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "命令参数无效",
			"data": nil,
		})
		return
	}

	// 生成脚本
	script := cmd.GenerateScript(req.Params...)

	cluster, shard, valid := taskbridge.ParseManagedSessionName(req.SessionName)
	if !valid {
		log.Printf("[API][TestTmuxCommand] 会话名称格式不正确: %s", req.SessionName)
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "会话名称格式不正确，应为 dstserver_存档名_世界名",
			"data": nil,
		})
		return
	}

	if legacyRuntimeConsole == nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "旧 tmux 命令已停用；任务必须迁移到受管 Room/Shard Runtime",
			"data": nil,
		})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := legacyRuntimeConsole.Send(ctx, cluster, shard, script); err != nil {
		log.Printf("[API][TestTmuxCommand] 发送命令失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "发送命令失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "命令发送成功",
		"data": map[string]interface{}{
			"session_name": req.SessionName,
			"command_id":   req.CommandID,
			"command_name": cmd.Name,
			"script":       script,
		},
	})
}

// TmuxRawCommandTestRequest 测试tmux原始命令请求
type TmuxRawCommandTestRequest struct {
	SessionName string `json:"session_name" binding:"required"` // 会话名称
	Command     string `json:"command" binding:"required"`      // 命令内容
}

// TestTmuxRawCommand 测试tmux原始命令
func TestTmuxRawCommand(c *gin.Context) {
	var req TmuxRawCommandTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	cluster, shard, valid := taskbridge.ParseManagedSessionName(req.SessionName)
	if !valid {
		log.Printf("[API][TestTmuxRawCommand] 会话名称格式不正确: %s", req.SessionName)
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "会话名称格式不正确，应为 dstserver_存档名_世界名",
			"data": nil,
		})
		return
	}

	if legacyRuntimeConsole == nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "旧 tmux 原始命令已停用；请使用受管 Runtime 命令接口",
			"data": nil,
		})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := legacyRuntimeConsole.Send(ctx, cluster, shard, req.Command); err != nil {
		log.Printf("[API][TestTmuxRawCommand] 发送命令失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "发送命令失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "命令发送成功",
		"data": map[string]interface{}{
			"session_name": req.SessionName,
			"command":      req.Command,
		},
	})
}
