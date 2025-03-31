package agent

import (
	"dont/controller"

	"github.com/gin-gonic/gin"
)

// GetAllAgents 获取所有已连接的Agent
func GetAllAgents(c *gin.Context) {
	controller.GetAllAgents(c)
}

// SendCommand 向指定Agent发送命令
func SendCommand(c *gin.Context) {
	controller.SendCommand(c)
}

// RequestReport 请求Agent上报信息
func RequestReport(c *gin.Context) {
	controller.RequestReport(c)
}

// GetSecurityKey 获取当前通信安全密钥
func GetSecurityKey(c *gin.Context) {
	controller.GetSecurityKey(c)
}

// GenerateNewKey 生成新的通信安全密钥
func GenerateNewKey(c *gin.Context) {
	controller.GenerateNewKey(c)
}

// UpdateSecurityKey 更新通信安全密钥
func UpdateSecurityKey(c *gin.Context) {
	controller.UpdateSecurityKey(c)
} 