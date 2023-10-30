package routers

import (
	"dont/controller"
	"dont/middleware/cors"
	"dont/middleware/jwt"
	"dont/pkg/setting"
	"dont/routers/api"
	"dont/routers/api/v1"
	"dont/routers/mod"
	"dont/routers/status"
	"dont/routers/user"
	"dont/routers/serverlog"
	"github.com/gin-gonic/gin"
)

func InitRouter() *gin.Engine {
	r := gin.New()

	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	//config := cors.DefaultConfig()
	//config.AllowOrigins = []string{"*"} // 允许来自指定域名的请求
	//config.AllowCredentials = true                                                                                                                    // 允许发送跨域凭据（例如 Cookie）
	//config.AllowOrigins = []s // 允许来自指定域名的请求
	//r.Use(cors.New(config))
	r.Use(cors.Cors())

	gin.SetMode(setting.RunMode)
	r.GET("/auth", api.GetAuth)

	apiv1 := r.Group("/api/v1")
	apiv1.Use(jwt.JWT())
	{
		//获取标签列表
		apiv1.GET("/tags", v1.GetTags)
		//新建标签
		apiv1.POST("/tags", v1.AddTag)
		//更新指定标签
		apiv1.PUT("/tags/:id", v1.EditTag)
		//删除指定标签
		apiv1.DELETE("/tags/:id", v1.DeleteTag)
	}
	apimod := r.Group("/mod")
	//apimod.Use(jwt.JWT())
	{
		apimod.GET("/search", mod.SearchMod)
		//apimod.GET("/add", mod.AddMod)
		apimod.POST("/down", mod.DownloadMod)
		apimod.GET("/down", mod.DownloadMod)
	}
	apistatus := r.Group("/status")
	//apimod.Use(jwt.JWT())
	apistatus.Use(controller.AuthMiddleWare())
	{

		apistatus.GET("/systeminfo", status.Cpuinfo)
	}
	users := r.Group("/user")
	{
		users.GET("/captcha/img", user.Img)
		users.GET("/account/info", user.Info)
		users.GET("/account/permmenu", user.Permmenu)
		users.POST("/login", user.Login)
		users.GET("/changepasswd", user.ChangePass)
		users.Use(controller.AuthMiddleWare())
		users.GET("/systeminfo", status.Cpuinfo)
	}
	ws := r.Group("/ws")
	{
		ws.GET("/serverlog",serverlog.Logtailf)
		//ws.GET("/log",serverlog.Lslog)
	}
	return r
}
