package routers

import (
	"dont/middleware"
	"dont/routers/agent"
	"dont/routers/auth"
	"dont/routers/backup"
	"dont/routers/dstcustomize"
	"dont/routers/dstserver"
	"dont/routers/mod"
	"dont/routers/server"
	"dont/routers/status"
	"dont/routers/tag"
	"dont/routers/user"
	"github.com/gin-gonic/gin"
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

		// 流式日志接口 - 不需要认证
		api.GET("/server/log/stream", server.StreamLog)

		// Dashboard
		dashboardGroup := api.Group("/dashboard")
		{
			dashboardGroup.GET("/", server.Status) // 临时使用server.Status替代
			dashboardGroup.GET("/status", status.Cpuinfo())
			dashboardGroup.GET("/docker/containers", status.GetDstDockerContainers())
		}

		// Mods
		mods := api.Group("/mod")
		{
			mods.GET("/search", mod.SearchMod)
			mods.GET("/download", mod.DownloadMod)
			mods.POST("/download", mod.DownloadMod) // 添加POST方法支持
			mods.GET("/log", server.ServerLog)      // 临时使用server.ServerLog替代
			mods.GET("/local", server.Status)       // 临时使用server.Status替代
			mods.DELETE("/local", server.Status)    // 临时使用server.Status替代
		}

		// DST服务器配置管理
		dstservers := api.Group("/dstserver")
		{
			// 基本服务器管理
			dstservers.GET("/list", dstserver.GetServerList)

			// cluster.ini管理
			dstservers.GET("/config", dstserver.GetServerConfig)
			dstservers.POST("/config", dstserver.UpdateServerConfig)

			// 管理员列表管理
			dstservers.GET("/adminlist", dstserver.GetAdminList)
			dstservers.POST("/adminlist", dstserver.UpdateAdminList)

			// 黑名单管理
			dstservers.GET("/blocklist", dstserver.GetBlockList)
			dstservers.POST("/blocklist", dstserver.UpdateBlockList)

			// 白名单管理
			dstservers.GET("/whitelist", dstserver.GetWhiteList)
			dstservers.POST("/whitelist", dstserver.UpdateWhiteList)

			// 服务器令牌管理
			dstservers.GET("/token", dstserver.GetClusterToken)
			dstservers.POST("/token", dstserver.UpdateClusterToken)
		}

		// DST游戏自定义配置管理
		dstcustom := api.Group("/dstcustomize")
		{
			dstcustom.GET("/customize", dstcustomize.GetCustomizeLua)
		}
		
		// 存档备份管理
		archiveBackup := api.Group("/backup")
		{
			// 创建存档备份
			archiveBackup.POST("/create", backup.CreateBackup())
			
			// 获取备份列表
			archiveBackup.GET("/list", backup.ListBackups())
			
			// 下载备份文件
			archiveBackup.GET("/download", backup.DownloadBackup())
			
			// 恢复备份
			archiveBackup.POST("/restore", backup.RestoreBackup())
			
			// 删除备份
			archiveBackup.POST("/delete", backup.DeleteBackup())
		}

		// Agent管理
		agents := api.Group("/agent")
		{
			// 获取所有已连接的Agent
			agents.GET("/list", agent.GetAllAgents)
			
			// 向指定Agent发送命令（需要认证）
			agents.POST("/command",  agent.SendCommand)
			
			// 请求Agent上报信息（需要认证）
			agents.POST("/report", agent.RequestReport)
			
			// 安全密钥管理（暂时不需要认证）
			agents.GET("/security/key", agent.GetSecurityKey)
			agents.POST("/security/key/generate", agent.GenerateNewKey)
			agents.POST("/security/key/update", agent.UpdateSecurityKey)

			// 获取命令执行结果
			agents.GET("/command/:command_id", agent.GetCommandResult)
			agents.GET("/command", agent.GetCommandResults)
		}
	}

	return router
}
