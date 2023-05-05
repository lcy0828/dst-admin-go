package controller

import (
	"dont/pkg/e"
	"dont/pkg/util"
	"github.com/gin-gonic/gin"
	"net/http"
	"time"
)

func AuthMiddleWare() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取客户端cookie并校验
		//fmt.Println(c)
		if cookie, err := c.Cookie("login"); err == nil {
			if cookie == "ok" {
				//c.Next()
				var code int
				var data interface{}
				code = e.SUCCESS
				//token := c.Query("token")
				token, _ := c.Cookie("token")
				if token == "" {
					code = e.INVALID_PARAMS
				} else {
					claims, err := util.ParseToken(token)
					if err != nil {
						code = e.ERROR_AUTH_CHECK_TOKEN_FAIL
					} else if time.Now().Unix() > claims.ExpiresAt {
						code = e.ERROR_AUTH_CHECK_TOKEN_TIMEOUT
					}
				}
				if code != e.SUCCESS {
					c.JSON(http.StatusUnauthorized, gin.H{
						"code": code,
						"msg":  e.GetMsg(code),
						"data": data,
					})
					c.Abort()
					return
				}
				c.Next()
				return
			}
		} else {
			var code int
			var data interface{}
			code = e.SUCCESS
			token := c.Query("token")
			if token == "" {
				code = e.INVALID_PARAMS
			} else {
				claims, err := util.ParseToken(token)
				if err != nil {
					code = e.ERROR_AUTH_CHECK_TOKEN_FAIL
				} else if time.Now().Unix() > claims.ExpiresAt {
					code = e.ERROR_AUTH_CHECK_TOKEN_TIMEOUT
				}
			}
			if code != e.SUCCESS {
				c.JSON(http.StatusUnauthorized, gin.H{
					"code": code,
					"msg":  e.GetMsg(code),
					"data": data,
				})
				c.Abort()
				return
			}
			c.Next()
		}
		// 返回错误
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登陆"})
		// 若验证不通过，不再调用后续的函数处理
		c.Abort()
		return
	}
}
