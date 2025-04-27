package world

import (
	"dont/models"
	"dont/pkg/e"
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

// GetWorldStateHistory 获取世界状态历史记录
func GetWorldStateHistory(c *gin.Context) {
	var req struct {
		ArchiveName string `json:"archive_name" binding:"required"`
		WorldName   string `json:"world_name" binding:"required"`
		Limit       int    `json:"limit"`
		Offset      int    `json:"offset"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 设置默认值
	if req.Limit <= 0 {
		req.Limit = 10
	}
	if req.Offset < 0 {
		req.Offset = 0
	}

	// 从数据库中获取世界状态历史记录
	worldStates, err := models.GetWorldStateHistory(req.ArchiveName, req.WorldName, req.Limit, req.Offset)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取世界状态历史记录失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "获取世界状态历史记录成功",
		"data": gin.H{
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
			"limit":        req.Limit,
			"offset":       req.Offset,
			"count":        len(worldStates),
			"records":      worldStates,
		},
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

	// 调用 cron 包中的 ReadWorldStateFile 函数
	output, err := models.CallCronFunction("read_world_state", req.ArchiveName, req.WorldName)
	if err != nil {
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
		// 获取世界状态历史记录
		worldGroup.POST("/state/history", GetWorldStateHistory)
		// 手动读取世界状态文件
		worldGroup.POST("/state/read", ReadWorldState)
	}
}
