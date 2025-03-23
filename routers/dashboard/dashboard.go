package dashboard

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

// DashboardInfo 获取仪表盘信息
func DashboardInfo(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "仪表盘功能待实现",
		"data": map[string]interface{}{
			"system_info": map[string]string{
				"cpu":    "暂无数据",
				"memory": "暂无数据",
				"disk":   "暂无数据",
			},
		},
	})
} 