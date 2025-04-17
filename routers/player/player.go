package player

import (
	"dont/cron"
	"dont/models"
	"dont/pkg/e"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
	"strconv"
)

// GetOnlinePlayers 获取在线玩家列表
func GetOnlinePlayers(c *gin.Context) {
	archiveName := c.Query("archive_name")
	// archive_name参数可选，如果不提供，则获取所有存档的在线玩家

	players, err := models.GetOnlinePlayers(archiveName)
	if err != nil {
		log.Printf("[API][GetOnlinePlayers] 获取在线玩家失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取在线玩家失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": players,
	})
}

// GetAllPlayers 获取所有玩家列表
func GetAllPlayers(c *gin.Context) {
	archiveName := c.Query("archive_name")
	// archive_name参数可选，如果不提供，则获取所有存档的玩家

	// 获取分页参数
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "10"))

	// 确保页码和每页数量有效
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 10
	}

	players, total, err := models.GetAllPlayers(archiveName, page, pageSize)
	if err != nil {
		log.Printf("[API][GetAllPlayers] 获取所有玩家失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取所有玩家失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":  e.SUCCESS,
		"msg":   e.GetMsg(e.SUCCESS),
		"data":  players,
		"total": total,
		"page":  page,
		"size":  pageSize,
	})
}

// GetPlayerStats 获取玩家统计信息
func GetPlayerStats(c *gin.Context) {
	archiveName := c.Query("archive_name")
	// archive_name参数可选，如果不提供，则获取所有存档的统计信息

	stats, err := models.GetPlayerStats(archiveName)
	if err != nil {
		log.Printf("[API][GetPlayerStats] 获取玩家统计信息失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取玩家统计信息失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": stats,
	})
}

// GetPlayerDetail 获取玩家详情
func GetPlayerDetail(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的玩家ID",
			"data": nil,
		})
		return
	}

	player, err := models.GetPlayerByID(id)
	if err != nil {
		log.Printf("[API][GetPlayerDetail] 获取玩家详情失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取玩家详情失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": player,
	})
}

// GetPlayerArchives 获取玩家数据库中存在的所有存档列表
func GetPlayerArchives(c *gin.Context) {
	log.Printf("[API][GetPlayerArchives] 开始获取玩家数据库中的存档列表")

	archives, err := models.GetPlayerArchives()
	if err != nil {
		log.Printf("[API][GetPlayerArchives] 获取存档列表失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取存档列表失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": archives,
	})
}

// UpdatePlayerInfo 手动更新玩家信息
func UpdatePlayerInfo(c *gin.Context) {
	var req struct {
		SessionName string `json:"session_name" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	log.Printf("[API][UpdatePlayerInfo] 开始手动更新玩家信息，会话名: %s", req.SessionName)

	// 直接调用 cron 包中的 UpdatePlayerInfo 函数

	// 直接调用 UpdatePlayerInfo 函数
	output, err := cron.UpdatePlayerInfo(req.SessionName)
	if err != nil {
		log.Printf("[API][UpdatePlayerInfo] 更新玩家信息失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "更新玩家信息失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "玩家信息更新成功",
		"data": gin.H{
			"output":       output,
			"session_name": req.SessionName,
		},
	})
}
