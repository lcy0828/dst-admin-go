package world

import (
	"dont/cron"
	"dont/models"
	"dont/pkg/e"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetWorldState 获取世界状态信息
func GetWorldState(c *gin.Context) {
	var req struct {
		ArchiveName string `json:"archive_name" binding:"required"`
		WorldName   string `json:"world_name" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 从数据库中获取最新的世界状态信息
	worldState, err := models.GetLatestWorldStateInfo(req.ArchiveName, req.WorldName)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取世界状态信息失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	if worldState == nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "未找到世界状态信息",
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "获取世界状态信息成功",
		"data": worldState,
	})
}

// ReadWorldState 手动读取世界状态文件
func ReadWorldState(c *gin.Context) {
	var req struct {
		ArchiveName string `json:"archive_name" binding:"required"`
		WorldName   string `json:"world_name" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	log.Printf("[API][ReadWorldState] 开始读取世界状态文件，存档: %s, 世界: %s",
		req.ArchiveName, req.WorldName)

	// 直接调用 cron 包中的 ReadWorldStateFile 函数
	output, err := cron.ReadWorldStateFile(req.ArchiveName, req.WorldName)
	if err != nil {
		log.Printf("[API][ReadWorldState] 读取世界状态文件失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "读取世界状态文件失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 从数据库中获取最新的世界状态信息
	worldState, err := models.GetLatestWorldStateInfo(req.ArchiveName, req.WorldName)
	if err != nil {
		log.Printf("[API][ReadWorldState] 获取世界状态信息失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取世界状态信息失败: " + err.Error(),
			"data": gin.H{
				"output":       output,
				"archive_name": req.ArchiveName,
				"world_name":   req.WorldName,
				"world_state":  nil,
			},
		})
		return
	}

	log.Printf("[API][ReadWorldState] 成功读取世界状态文件，存档: %s, 世界: %s",
		req.ArchiveName, req.WorldName)

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "读取世界状态文件成功",
		"data": gin.H{
			"output":       output,
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
			"world_state":  worldState,
		},
	})
}

// RegisterWorldRoutes 注册世界相关路由
func RegisterWorldRoutes(router *gin.RouterGroup) {
	worldGroup := router.Group("/world")
	{
		// 获取世界状态信息
		worldGroup.POST("/state", GetWorldState)
		// 手动读取世界状态文件
		worldGroup.POST("/state/read", ReadWorldState)
	}
}
