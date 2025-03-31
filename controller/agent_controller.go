package controller

import (
	"dont/server"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// 全局Agent服务器实例
var AgentServer *server.Server

// GetAllAgents 获取所有已连接的Agent
func GetAllAgents(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code": 503,
			"msg":  "Agent服务器未启动，请使用--agent-server参数启动服务",
			"data": nil,
		})
		return
	}

	agents := AgentServer.GetAllAgentInfo()
	
	// 当没有Agent连接时返回空数组而不是null
	if agents == nil {
		agents = make(map[string]map[string]interface{})
	}
	
	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "获取成功",
		"data": agents,
	})
}

// SendCommand 向指定Agent发送命令
func SendCommand(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": 500,
			"msg":  "Agent服务器未启动",
			"data": nil,
		})
		return
	}

	var req struct {
		AgentID  string `json:"agent_id" binding:"required"`
		Type     string `json:"type" binding:"required"`
		Content  string `json:"content" binding:"required"`
		Timeout  int    `json:"timeout"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": 400,
			"msg":  "参数错误: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 检查命令类型
	if req.Type != "shell" && req.Type != "script" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": 400,
			"msg":  "不支持的命令类型，仅支持shell和script",
			"data": nil,
		})
		return
	}

	// 如果未指定超时，设置默认值
	if req.Timeout <= 0 {
		if req.Type == "shell" {
			req.Timeout = 30 // shell命令默认30秒超时
		} else {
			req.Timeout = 60 // 脚本默认60秒超时
		}
	}

	// 发送命令
	commandID, err := AgentServer.SendCommand(req.AgentID, req.Type, req.Content, req.Timeout)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": 500,
			"msg":  "发送命令失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "命令已发送",
		"data": gin.H{
			"command_id": commandID,
		},
	})
}

// GetCommandResult 获取命令执行结果
func GetCommandResult(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": 500,
			"msg":  "Agent服务器未启动",
			"data": nil,
		})
		return
	}

	// 从URL获取命令ID
	commandID := c.Param("command_id")
	if commandID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": 400,
			"msg":  "缺少命令ID参数",
			"data": nil,
		})
		return
	}

	// 查询命令结果
	result, err := AgentServer.GetCommandResult(commandID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code": 404,
			"msg":  err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "获取成功",
		"data": result,
	})
}

// GetCommandResults 获取命令执行结果列表
func GetCommandResults(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": 500,
			"msg":  "Agent服务器未启动",
			"data": nil,
		})
		return
	}

	// 获取查询参数
	agentID := c.Query("agent_id")
	limitStr := c.Query("limit")
	
	limit := 20 // 默认限制20条
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			limit = 20
		}
	}

	// 获取结果列表
	results := AgentServer.GetCommandResults(agentID, limit)

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "获取成功",
		"data": results,
	})
}

// RequestReport 请求Agent上报信息
func RequestReport(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": 500,
			"msg":  "Agent服务器未启动",
			"data": nil,
		})
		return
	}

	var req struct {
		AgentID    string                 `json:"agent_id" binding:"required"`
		ReportType string                 `json:"report_type" binding:"required"`
		Params     map[string]interface{} `json:"params"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": 400,
			"msg":  "参数错误: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 如果未提供参数，初始化空map
	if req.Params == nil {
		req.Params = make(map[string]interface{})
	}

	// 请求上报
	err := AgentServer.RequestPassiveReport(req.AgentID, req.ReportType, req.Params)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": 500,
			"msg":  "请求上报失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "上报请求已发送",
		"data": nil,
	})
}

// GetSecurityKey 获取当前的通信安全密钥
func GetSecurityKey(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code": 503,
			"msg":  "Agent服务器未启动，请使用--agent-server参数启动服务",
			"data": nil,
		})
		return
	}

	// 从服务器获取当前通信密钥
	key := AgentServer.GetSecurityKey()
	
	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "获取成功",
		"data": gin.H{
			"key": key,
		},
	})
}

// GenerateNewKey 生成新的通信安全密钥
func GenerateNewKey(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Agent Server未启动",
		})
		return
	}

	err := AgentServer.GenerateNewSecurityKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("生成新密钥失败: %v", err),
		})
		return
	}

	// 获取新生成的密钥
	key := AgentServer.GetSecurityKey()
	log.Printf("已生成新的通信密钥: %s", key)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "已成功生成新密钥",
		"key":     key,
	})
}

// UpdateSecurityKey 更新通信安全密钥
func UpdateSecurityKey(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Agent Server未启动",
		})
		return
	}

	var request struct {
		Key string `json:"key" binding:"required"`
	}

	if err := c.BindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "无效的请求参数",
		})
		return
	}

	err := AgentServer.UpdateSecurityKey(request.Key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("更新密钥失败: %v", err),
		})
		return
	}

	log.Printf("已更新通信密钥: %s", request.Key)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "已成功更新密钥",
	})
} 