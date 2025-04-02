package cors

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	//"net/http"
	"time"
)

func Cors() gin.HandlerFunc {
	// 选择一种方式允许所有源
	// 方法1: 使用 AllowAllOrigins，但不支持凭证
	/*
		return cors.New(cors.Config{
			AllowAllOrigins: true,
			AllowMethods:    []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
			AllowHeaders:    []string{"Origin", "X-Requested-With", "X-Extra-Header", "Content-Type", "Accept", "Authorization"},
			ExposeHeaders:   []string{"Content-Length", "Authorization", "Content-Type", "Access-Control-Allow-Origin", "Access-Control-Allow-Headers"},
			AllowCredentials: false, // 当 AllowAllOrigins 为 true 时，这里必须设置为 false
			MaxAge:           12 * time.Hour,
		})
	*/

	// 方法2: 使用 AllowOriginFunc，可以支持凭证
	return cors.New(cors.Config{
		// 不要设置 AllowAllOrigins，而是使用 AllowOriginFunc
		AllowOriginFunc: func(origin string) bool {
			return true // 允许所有源，并支持凭证
		},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowHeaders:     []string{"Origin", "X-Requested-With", "X-Extra-Header", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders:    []string{"Content-Length", "Authorization", "Content-Type", "Access-Control-Allow-Origin", "Access-Control-Allow-Headers"},
		AllowCredentials: true, // 当使用 AllowOriginFunc 时，可以设置为 true
		MaxAge:           12 * time.Hour,
	})
}

//func Cors() gin.HandlerFunc {
//	return func(c *gin.Context) {
//		method := c.Request.Method
//		origin := c.Request.Header.Get("Origin") //请求头部
//		if origin != "" {
//			// 当Access-Control-Allow-Credentials为true时
//			c.Header("Access-Control-Allow-Origin", "http://dont.lcy.pub")
//			c.Header("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE, UPDATE")
//			c.Header("Access-Control-Allow-Headers", "Origin, X-Requested-With, X-Extra-Header, Content-Type, Accept, Authorization")
//			c.Header("Access-Control-Expose-Headers", "Content-Length, Access-Control-Allow-Origin, Access-Control-Allow-Headers, Cache-Control, Content-Language, Content-Type")
//			c.Header("Access-Control-Allow-Credentials", "true")
//
//		}
//
//		if method == "OPTIONS" {
//			c.AbortWithStatus(http.StatusNoContent)
//		}
//
//		c.Next()
//	}
//}
