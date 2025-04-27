package player

import (
	"dont/cron"
	"dont/models"
	"dont/pkg/e"
	"dont/pkg/types"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"log"
	"net/http"
	"os"
	"path/filepath"
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

// GetHostInfo 从数据库中获取主机信息
func GetHostInfo(c *gin.Context) {
	var req struct {
		ArchiveName string `json:"archive_name" binding:"required"`
		WorldName   string `json:"world_name"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	log.Printf("[API][GetHostInfo] 开始从数据库中获取主机信息，存档: %s, 世界: %s",
		req.ArchiveName, req.WorldName)

	// 从数据库中获取主机信息
	hosts, err := models.GetHostInfo(req.ArchiveName, req.WorldName)
	if err != nil {
		log.Printf("[API][GetHostInfo] 获取主机信息失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取主机信息失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "获取主机信息成功",
		"data": gin.H{
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
			"host_count":   len(hosts),
			"hosts":        hosts,
		},
	})
}

// GetHostInfoList 获取主机信息列表
func GetHostInfoList(c *gin.Context) {
	// 获取查询参数
	archiveName := c.Query("archive_name")
	worldName := c.Query("world_name")

	log.Printf("[API][GetHostInfoList] 开始获取主机信息列表，存档: %s, 世界: %s",
		archiveName, worldName)

	// 从数据库中获取主机信息
	hosts, err := models.GetHostInfo(archiveName, worldName)
	if err != nil {
		log.Printf("[API][GetHostInfoList] 获取主机信息失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取主机信息失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "获取主机信息列表成功",
		"data": gin.H{
			"archive_name": archiveName,
			"world_name":   worldName,
			"host_count":   len(hosts),
			"hosts":        hosts,
		},
	})
}

// GetPlayerConfig 从数据库中获取玩家配置信息
func GetPlayerConfig(c *gin.Context) {
	var req struct {
		ArchiveName string `json:"archive_name" binding:"required"`
		WorldName   string `json:"world_name"`
		FilterMode  int    `json:"filter_mode"` // 过滤模式，0=全部显示，1=只显示主机，2=只显示玩家
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 验证过滤模式
	if req.FilterMode < 0 || req.FilterMode > 2 {
		req.FilterMode = 0 // 默认显示全部
	}

	// 输出过滤模式信息
	var modeDesc string
	switch req.FilterMode {
	case 0:
		modeDesc = "全部"
	case 1:
		modeDesc = "只显示主机"
	case 2:
		modeDesc = "只显示玩家"
	}

	log.Printf("[API][GetPlayerConfig] 开始从数据库中获取玩家配置信息，存档: %s, 世界: %s, 模式: %s",
		req.ArchiveName, req.WorldName, modeDesc)

	// 从数据库中获取玩家配置信息
	players, err := models.GetPlayerConfigInfo(req.ArchiveName, req.WorldName, req.FilterMode)
	if err != nil {
		log.Printf("[API][GetPlayerConfig] 获取玩家配置信息失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "获取玩家配置信息失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "获取玩家配置信息成功",
		"data": gin.H{
			"archive_name": req.ArchiveName,
			"world_name":   req.WorldName,
			"filter_mode":  req.FilterMode,
			"player_count": len(players),
			"players":      players,
		},
	})
}

// ReadPlayerConfig 手动读取玩家配置文件
func ReadPlayerConfig(c *gin.Context) {
	var req struct {
		ArchiveName string `json:"archive_name" binding:"required"`
		WorldName   string `json:"world_name" binding:"required"`
		FilterMode  int    `json:"filter_mode"` // 过滤模式，0=全部显示，1=只显示主机，2=只显示玩家
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 验证过滤模式
	if req.FilterMode < 0 || req.FilterMode > 2 {
		req.FilterMode = 0 // 默认显示全部
	}

	// 输出过滤模式信息
	var modeDesc string
	switch req.FilterMode {
	case 0:
		modeDesc = "全部"
	case 1:
		modeDesc = "只显示主机"
	case 2:
		modeDesc = "只显示玩家"
	}

	log.Printf("[API][ReadPlayerConfig] 开始读取玩家配置文件，存档: %s, 世界: %s, 模式: %s",
		req.ArchiveName, req.WorldName, modeDesc)

	// 直接调用 cron 包中的 ReadPlayerConfigFile 函数
	output, err := cron.ReadPlayerConfigFile(req.ArchiveName, req.WorldName, req.FilterMode)
	if err != nil {
		log.Printf("[API][ReadPlayerConfig] 读取玩家配置文件失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "读取玩家配置文件失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 获取存档路径
	dstSavePath := getDstSavePath()
	log.Printf("[API][ReadPlayerConfig] 存档路径: %s", dstSavePath)

	// 构建玩家配置文件路径
	playerConfigPath := filepath.Join(dstSavePath, req.ArchiveName, req.WorldName, "save", "mod_config_data", "players")
	log.Printf("[API][ReadPlayerConfig] 玩家配置文件路径: %s", playerConfigPath)

	// 读取文件并解析详细信息
	fileContent, err := os.ReadFile(playerConfigPath)
	if err != nil {
		log.Printf("[API][ReadPlayerConfig] 读取玩家配置文件失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "读取玩家配置文件失败: " + err.Error(),
			"data": gin.H{
				"output":        output,
				"archive_name":  req.ArchiveName,
				"world_name":    req.WorldName,
				"config_path":   playerConfigPath,
				"detailed_info": nil,
			},
		})
		return
	}

	// 解析玩家配置文件
	players, err := cron.ParsePlayerConfigString(string(fileContent))
	if err != nil {
		log.Printf("[API][ReadPlayerConfig] 解析玩家配置文件失败: %v", err)
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "解析玩家配置文件失败: " + err.Error(),
			"data": gin.H{
				"output":        output,
				"archive_name":  req.ArchiveName,
				"world_name":    req.WorldName,
				"config_path":   playerConfigPath,
				"detailed_info": nil,
			},
		})
		return
	}

	// 将玩家配置信息保存到数据库
	if err := models.SavePlayerConfigInfo(req.ArchiveName, req.WorldName, players); err != nil {
		log.Printf("[API][ReadPlayerConfig] 保存玩家配置信息到数据库失败: %v", err)
		// 不返回错误，继续处理
	}

	// 根据过滤模式过滤玩家列表
	var displayPlayers []types.PlayerConfigInfo
	switch req.FilterMode {
	case 1: // 只显示主机
		for _, player := range players {
			if player.IsHost {
				displayPlayers = append(displayPlayers, player)
			}
		}
	case 2: // 只显示玩家
		for _, player := range players {
			if !player.IsHost {
				displayPlayers = append(displayPlayers, player)
			}
		}
	default: // 显示全部
		displayPlayers = players
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "读取玩家配置文件成功",
		"data": gin.H{
			"output":        output,
			"archive_name":  req.ArchiveName,
			"world_name":    req.WorldName,
			"config_path":   playerConfigPath,
			"player_count":  len(displayPlayers),
			"detailed_info": displayPlayers,
			"total_count":   len(players),
			"filter_mode":   req.FilterMode,
		},
	})
}

// getDstSavePath 获取DST存档路径
func getDstSavePath() string {
	// 默认路径
	dstSavePath := "./Klei/DoNotStarveTogether"

	// 直接使用配置文件中的路径
	configFile := "./conf/app.conf"
	log.Printf("[API][getDstSavePath] 尝试读取配置文件: %s", configFile)

	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		log.Printf("[API][getDstSavePath] 配置文件存在")
		if cfg, err := ini.Load(configFile); err == nil {
			log.Printf("[API][getDstSavePath] 成功加载配置文件")
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
				dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
				log.Printf("[API][getDstSavePath] 从配置文件加载DST存档路径: %s", dstSavePath)
				return dstSavePath
			} else {
				log.Printf("[API][getDstSavePath] 配置文件中没有 DST_SAVE_PATH 配置")
			}
		} else {
			log.Printf("[API][getDstSavePath] 加载配置文件失败: %v", err)
		}
	} else {
		log.Printf("[API][getDstSavePath] 配置文件不存在: %s", configFile)
	}

	// 如果配置文件不存在或者没有配置，尝试使用环境变量
	if envPath := os.Getenv("DST_SAVE_PATH"); envPath != "" {
		dstSavePath = envPath
		log.Printf("[API][getDstSavePath] 从环境变量加载DST存档路径: %s", dstSavePath)
		return dstSavePath
	}

	// 如果环境变量也没有设置，使用默认路径
	log.Printf("[API][getDstSavePath] 使用默认路径: %s", dstSavePath)
	return dstSavePath
}
