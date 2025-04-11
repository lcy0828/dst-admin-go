package routers

import (
	"dont/middleware"
	"dont/routers/agent"
	"dont/routers/auth"
	"dont/routers/backup"
	"dont/routers/dstcustomize"
	"dont/routers/dstserver"
	"dont/routers/gamelog"
	"dont/routers/mod"
	"dont/routers/parser"
	"dont/routers/server"
	"dont/routers/status"
	"dont/routers/tag"
	"dont/routers/tmux"
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

	// 静态文件服务
	router.StaticFile("/gamelog", "./static/gamelog.html")
	router.Static("/static", "./static")

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

		// 游戏日志实时监控接口 - 使用WebSocket
		api.GET("/game/log/ws", gamelog.HandleLogWebSocket)

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
			// 新增服务器模组管理API
			mods.POST("/server/add", mod.AddModToServer)
			mods.POST("/server/update", mod.AddModToServer) // 添加模组到服务器
			mods.GET("/server/list", mod.GetServerMods)     // 获取服务器模组列表
			mods.GET("/config", mod.GetModConfig)           // 获取模组配置信息
			// 新增模组自定义配置管理API
			mods.POST("/custom-config", mod.SaveModCustomConfig) // 保存模组自定义配置
			mods.GET("/custom-config", mod.GetModCustomConfig)   // 获取模组自定义配置
			mods.GET("/config-file", mod.GenerateModConfigFile)  // 生成模组配置文件
			// 新增模组删除和启用/禁用API
			mods.POST("/server/delete", mod.DeleteServerMod) // 删除服务器模组
			mods.POST("/toggle", mod.ToggleModEnabled)       // 启用/禁用模组
		}

		// DST服务器配置管理
		dstservers := api.Group("/dstserver")
		{
			// 基本服务器管理
			dstservers.GET("/list", dstserver.GetServerList)

			// cluster.ini配置管理
			dstservers.GET("/clusterconfig", dstserver.GetClusterConfig)
			dstservers.POST("/clusterconfig", dstserver.UpdateClusterConfig)

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

			// 获取饥荒最新版本
			dstservers.GET("/version", dstserver.GetDSTVersion)

			// 获取本地安装的饥荒版本
			dstservers.GET("/localversion", dstserver.GetLocalDSTVersion)

			// 获取世界配置中的overrides字段
			dstservers.GET("/worldoverrides", dstserver.GetWorldOverrides)

			// POST方法的worldoverrides接口已删除

			// 创建或更新森林世界的leveldataoverride.lua文件
			dstservers.POST("/forestworld", dstserver.CreateForestWorld)

			// 创建或更新洞穴世界的leveldataoverride.lua文件
			dstservers.POST("/caveworld", dstserver.CreateCaveWorld)

			// 从模板创建森林世界的leveldataoverride.lua文件
			dstservers.POST("/template/forestworld", dstserver.CreateTemplateForestWorld)

			// 从模板创建洞穴世界的leveldataoverride.lua文件
			dstservers.POST("/template/caveworld", dstserver.CreateTemplateCaveWorld)

			// 获取server.ini文件
			dstservers.GET("/serverini", dstserver.GetServerIni)

			// 创建或更新server.ini文件
			dstservers.POST("/serverini", dstserver.CreateOrUpdateServerIni)

			// 删除世界
			dstservers.POST("/deleteworld", dstserver.DeleteWorld)
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
			agents.POST("/command", agent.SendCommand)

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

		// 日志解析器API
		parserGroup := api.Group("/v1")
		{
			// 注册现有的日志解析器API
			gamelog.RegisterParserAPIRoutes(parserGroup)

			// 添加新的日志解析器API
			parserGroup.GET("/parser/active", parser.GetActiveParsers) // 获取当前运行中的解析器
		}

		// Tmux服务器管理API
		tmuxGroup := api.Group("/tmux")
		{
			tmuxGroup.POST("/start", tmux.StartServer)
			tmuxGroup.POST("/stop", tmux.StopServer)
			tmuxGroup.POST("/command", tmux.HandleCommand)        // 模块化命令API
			tmuxGroup.POST("/raw-command", tmux.HandleRawCommand) // 原始命令API（保持向后兼容）
			tmuxGroup.GET("/list", tmux.ListServers)
			tmuxGroup.POST("/kill", tmux.KillServer)
			tmuxGroup.GET("/debug", tmux.DebugTmuxSessions) // 调试端点，输出会话信息到日志

			// 命令管理API
			commandGroup := tmuxGroup.Group("/commands")
			{
				commandGroup.GET("", tmux.ListCommands)             // 获取命令列表
				commandGroup.POST("/detail", tmux.GetCommandDetail) // 获取单个命令
				commandGroup.POST("", tmux.AddCommand)              // 添加命令
				commandGroup.POST("/update", tmux.UpdateCommand)    // 更新命令
				commandGroup.POST("/delete", tmux.DeleteCommand)    // 删除命令
			}
		}
	}

	return router
}
