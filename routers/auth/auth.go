package auth

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

// Login 处理登录请求
func Login(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "登录功能待实现",
	})
}

// Register 处理注册请求
func Register(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "注册功能待实现",
	})
}

// CheckToken 检查token有效性
func CheckToken(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "Token检查功能待实现",
	})
} 