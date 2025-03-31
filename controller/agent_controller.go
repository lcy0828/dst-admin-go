package controller

import (
	"dont/server"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 全局Agent服务器实例
var AgentServer *server.Server

// GetAllAgents 获取所有已连接的Agent
func GetAllAgents(c *gin.Context) {
	if AgentServer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": 500,
			"msg":  "Agent服务器未启动",
			"data": nil,
		})
		return
	}

	agents := AgentServer.GetAllAgentInfo()
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