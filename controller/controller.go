package controller

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

func AuthMiddleWare() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取客户端cookie并校验
		if cookie, err := c.Cookie("login"); err == nil {
			if cookie == "ok" {
				c.Next()
				return
			}
		}
		// 返回错误
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登陆"})
		// 若验证不通过，不再调用后续的函数处理
		c.Abort()
		return
	}
}
