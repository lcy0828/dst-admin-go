package cors

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	//"net/http"
	"time"
)

func Cors() gin.HandlerFunc {
	return cors.New(cors.Config{
		//AllowAllOrigins:  true,
		AllowOrigins:  []string{"http://dont.lcy.pub", "https://dont.lcy.pub", "http://dont.lcy.pub:5173", "http://192.168.2.32:8080", "http://192.168.2.22:8080"},
		AllowMethods:  []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:  []string{"Origin", "X-Requested-With", "X-Extra-Header", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders: []string{"*"},
		//ExposeHeaders:    []string{"Content-Length", "Authorization", "Content-Type","Access-Control-Allow-Origin","Access-Control-Allow-Headers","Cache-Control","Content-Language"},
		AllowCredentials: true,
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
