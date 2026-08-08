package controller

import (
	"crypto/sha256"
	"dont/server"
	"encoding/hex"
	"log"
	"net/http"
	"strconv"
	"strings"

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
	legacyAgentGone(c, "旧 Agent 任意命令接口已停用", "/api/v2/agents/:agentId/commands")
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

	log.Printf("获取命令执行结果，CommandID: %s", commandID)

	// 查询命令结果
	result, err := AgentServer.GetCommandResult(commandID)
	if err != nil {
		log.Printf("获取命令结果失败: %v", err)
		c.JSON(http.StatusNotFound, gin.H{
			"code": 404,
			"msg":  err.Error(),
			"data": nil,
		})
		return
	}

	// 验证结果格式
	if result.CommandID == "" {
		log.Printf("警告：返回的命令结果缺少CommandID")
	}

	log.Printf("成功获取命令结果: %s, AgentID: %s, Status: %s", result.CommandID, result.AgentID, result.Status)

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

	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": 400,
			"msg":  "缺少必要的agent_id参数",
			"data": nil,
		})
		return
	}

	log.Printf("获取命令执行结果列表，AgentID: %s", agentID)

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

	if len(results) == 0 {
		log.Printf("未找到Agent(%s)的命令结果", agentID)
		// 返回空数组而不是null
		c.JSON(http.StatusOK, gin.H{
			"code": 200,
			"msg":  "获取成功",
			"data": []interface{}{},
		})
		return
	}

	log.Printf("成功获取Agent(%s)的命令结果，共%d条", agentID, len(results))

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "获取成功",
		"data": results,
	})
}

// RequestReport 请求Agent上报信息
func RequestReport(c *gin.Context) {
	legacyAgentGone(c, "旧 Agent 自定义上报接口已停用", "/api/v2/agents/:agentId/commands")
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

	key := AgentServer.GetSecurityKey()
	digest := sha256.Sum256([]byte(key))
	masked := "********"
	if len(key) > 8 {
		masked = key[:4] + strings.Repeat("*", 12) + key[len(key)-4:]
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "获取成功",
		"data": gin.H{
			"masked_key":  masked,
			"fingerprint": hex.EncodeToString(digest[:]),
		},
	})
}

// GenerateNewKey 生成新的通信安全密钥
func GenerateNewKey(c *gin.Context) {
	legacyAgentGone(c, "旧密钥生成接口已停用", "/api/v2/agents/security/actions/rotate")
}

// UpdateSecurityKey 更新通信安全密钥
func UpdateSecurityKey(c *gin.Context) {
	legacyAgentGone(c, "旧密钥写入接口已停用", "/api/v2/agents/security/actions/rotate")
}

func legacyAgentGone(c *gin.Context, message, replacement string) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusGone, gin.H{
		"code": http.StatusGone,
		"msg":  message,
		"data": gin.H{"replacement": replacement},
	})
}
