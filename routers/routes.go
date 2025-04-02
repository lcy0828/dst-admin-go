package routers

import (
	"dst-admin-go/routers/backup"
	"dst-admin-go/routers/dstserver"
	"dst-admin-go/routers/mod"
	"dst-admin-go/routers/server"
	"dst-admin-go/routers/serverlog"
	"dst-admin-go/routers/tmux"
	"github.com/gin-gonic/gin"
)

// InitRouter 初始化路由
func InitRouter() *gin.Engine {
	r := gin.Default()

	// 设置静态文件目录
	r.Static("/static", "./static")
	r.StaticFile("/favicon.ico", "./static/favicon.ico")

	// 加载HTML模板
	r.LoadHTMLGlob("templates/*")

	// 首页路由
	r.GET("/", func(c *gin.Context) {
		c.HTML(200, "index.html", gin.H{
			"title": "DST-Admin-Go",
		})
	})

	// API路由组
	api := r.Group("/api")
	{
		// 服务器管理API
		api.GET("/server/list", dstserver.GetServerList)
		api.GET("/server/token", dstserver.GetServerToken)
		api.POST("/server/config", dstserver.UpdateServerConfig)
		api.GET("/server/log", server.StreamLog)

		// 模组管理API
		api.GET("/mod/search", mod.SearchMod)
		api.GET("/mod/download", mod.DownloadMod)
		api.POST("/mod/vote", mod.VoteMod)

		// 备份管理API
		api.GET("/backup/list", backup.ListBackups)
		api.POST("/backup/create", backup.CreateBackup)
		api.GET("/backup/download", backup.DownloadBackup)
		api.POST("/backup/restore", backup.RestoreBackup)
		api.DELETE("/backup/delete", backup.DeleteBackup)

		// 服务器日志API
		api.GET("/serverlog/tail", serverlog.TailLog)

		// Tmux服务器管理API
		api.POST("/tmux/start", tmux.StartServer)
		api.POST("/tmux/stop", tmux.StopServer)
		api.POST("/tmux/command", tmux.SendCommand)
		api.GET("/tmux/list", tmux.ListServers)
		api.POST("/tmux/kill", tmux.KillServer)
	}

	return r
}
