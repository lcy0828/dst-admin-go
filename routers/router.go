package routers

import (
	"dont/controller"
	"dont/middleware/jwt"
	"dont/pkg/setting"
	"dont/routers/api"
	"dont/routers/api/v1"
	"dont/routers/mod"
	"dont/routers/status"
	"dont/routers/user"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func InitRouter() *gin.Engine {
	r := gin.New()

	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	config := cors.DefaultConfig()
	config.AllowOrigins = []string{"http://192.168.40.9:8080"} // 允许来自指定域名的请求
	config.AllowCredentials = true                             // 允许发送跨域凭据（例如 Cookie）
	r.Use(cors.New(config))

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
	return r
}
