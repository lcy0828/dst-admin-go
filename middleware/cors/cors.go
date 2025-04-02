package cors

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	//"net/http"
	"time"
)

func Cors() gin.HandlerFunc {
	// 注意: AllowAllOrigins 和 AllowCredentials 同时为 true 可能会导致某些浏览器拒绝请求
	// 因为这违反了 CORS 规范
	// 如果需要支持凭证，则使用 AllowOriginFunc 或者指定具体的源
	return cors.New(cors.Config{
		// 选择一种方式允许所有源
		AllowAllOrigins: true, // 允许所有源，但不支持凭证
		// 或者使用下面的方式允许所有源并支持凭证
		AllowOriginFunc: func(origin string) bool {
			return true // 允许所有源，并支持凭证
		},
		AllowMethods:  []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowHeaders:  []string{"Origin", "X-Requested-With", "X-Extra-Header", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders: []string{"Content-Length", "Authorization", "Content-Type", "Access-Control-Allow-Origin", "Access-Control-Allow-Headers"},
		// 如果需要支持凭证，请使用 AllowOriginFunc 而不是 AllowAllOrigins
		AllowCredentials: false, // 当 AllowAllOrigins 为 true 时，这里应该设置为 false
		MaxAge:           12 * time.Hour,
	},
	)
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
