package routers

import (
	"github.com/gin-gonic/gin"
	"dont/middleware"
	"dont/routers/auth"
	"dont/routers/dstserver"
	"dont/routers/mod"
	"dont/routers/server"
	"dont/routers/tag"
	"dont/routers/user"
)

// InitRouter 初始化路由
func InitRouter() *gin.Engine {
	router := gin.New()

	//use middleware
	router.Use(
		gin.Logger(),
		gin.Recovery(),
		middleware.CorsMiddleware(),
	)

	api := router.Group("/api")
	{
		// Auth
		auths := api.Group("/auth")
		{
			auths.POST("/login", auth.Login)
			auths.POST("/register", auth.Register)
			auths.GET("/status", auth.CheckToken)
		}

		// Tags
		tagss := api.Group("/tags")
		{
			tagss.POST("/", middleware.JWTAuth(), tag.AddTag)
			tagss.DELETE("/:id", middleware.JWTAuth(), tag.DeleteTag)
			tagss.PUT("/", middleware.JWTAuth(), tag.EditTag)
			tagss.GET("/", tag.GetTags)
		}

		// Users
		users := api.Group("/user")
		{
			users.GET("/info", middleware.JWTAuth(), user.GetInfo)
			users.POST("/info", middleware.JWTAuth(), user.EditInfo)
		}

		// Server Logs
		serlogs := api.Group("/server").Use(middleware.JWTAuth())
		{
			serlogs.GET("/log", server.ServerLog)
			serlogs.POST("/log", server.ServerLog)
			serlogs.GET("/status", server.Status)
			serlogs.POST("/status", server.Status)
			serlogs.GET("/log/download", server.DownloadLog)
		}

		// Dashboard
		dashboardGroup := api.Group("/dashboard")
		{
			dashboardGroup.GET("/", server.Status) // 临时使用server.Status替代
		}

		// Mods
		mods := api.Group("/mod")
		{
			mods.GET("/search/:keyword/:page", mod.SearchMod)
			mods.GET("/download", mod.DownloadMod)
			mods.GET("/log", server.ServerLog) // 临时使用server.ServerLog替代
			mods.GET("/local", server.Status) // 临时使用server.Status替代
			mods.DELETE("/local", server.Status) // 临时使用server.Status替代
		}
		
		// DST服务器配置管理
		dstservers := api.Group("/dstserver")
		{
			dstservers.GET("/list", dstserver.GetServerList)
			dstservers.GET("/config/:savename", dstserver.GetServerConfig)
			dstservers.POST("/config/:savename", dstserver.UpdateServerConfig)
		}
	}

	return router
}
