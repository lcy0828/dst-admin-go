package dstserver

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 配置常量
const (
	dstSavePath = "/root/DST/Klei/DoNotStarveTogether" // DST存档目录
)

// DSTServerConfig 服务器配置结构
type DSTServerConfig struct {
	// STEAM部分
	SteamGroupID      string `json:"steam_group_id"`
	SteamGroupAdmins  bool   `json:"steam_group_admins"`
	SteamGroupOnly    bool   `json:"steam_group_only"`
	
	// GAMEPLAY部分
	GameMode         string `json:"game_mode"`
	PauseWhenEmpty   bool   `json:"pause_when_empty"`
	VoteEnabled      bool   `json:"vote_enabled"`
	PVP              bool   `json:"pvp"`
	MaxPlayers       int    `json:"max_players"`
	
	// NETWORK部分
	ClusterName        string `json:"cluster_name"`
	ClusterDescription string `json:"cluster_description"`
	ClusterIntention   string `json:"cluster_intention"`
	ClusterLanguage    string `json:"cluster_language"`
	WhitelistSlots     int    `json:"whitelist_slots"`
	IdleTimeout        int    `json:"idle_timeout"`
	ClusterPassword    string `json:"cluster_password"`
	LanOnlyCluster     bool   `json:"lan_only_cluster"`
	OfflineCluster     bool   `json:"offline_cluster"`
	AutosaverEnabled   bool   `json:"autosaver_enabled"`
	TickRate           int    `json:"tick_rate"`
	
	// MISC部分
	MaxSnapshots   int  `json:"max_snapshots"`
	ConsoleEnabled bool `json:"console_enabled"`
	
	// SHARD部分
	MasterIP      string `json:"master_ip"`
	ShardEnabled  bool   `json:"shard_enabled"`
	BindIP        string `json:"bind_ip"`
	MasterPort    int    `json:"master_port"`
	ClusterKey    string `json:"cluster_key"`
}

// ListResponse 列表响应结构
type ListResponse struct {
	Status int      `json:"status"`
	Data   []string `json:"data"`
}

// TokenResponse 服务器Token响应结构
type TokenResponse struct {
	Status int    `json:"status"`
	Data   string `json:"data"`
}

// 服务器列表响应
type ServerListResponse struct {
	Status int               `json:"status"`
	Data   []ServerShortInfo `json:"data"`
}

// 服务器简要信息
type ServerShortInfo struct {
	Name     string       `json:"name"`     // 存档名称
	SavePath string       `json:"savepath"` // 存档路径
	Worlds   []WorldInfo  `json:"worlds"`   // 世界列表
}

// 世界信息
type WorldInfo struct {
	Name     string `json:"name"`  // 世界名称
	Type     string `json:"type"`  // 世界类型 (forest/cave)
}

// 服务器详细信息响应
type ServerDetailResponse struct {
	Status int             `json:"status"`
	Data   DSTServerConfig `json:"data"`
}

// GetServerList 获取所有服务器存档列表
func GetServerList(g *gin.Context) {
	// 检查存档目录是否存在
	if _, err := os.Stat(dstSavePath); os.IsNotExist(err) {
		g.JSON(http.StatusOK, ServerListResponse{
			Status: 404,
			Data:   nil,
		})
		return
	}
	
	// 读取存档目录
	files, err := ioutil.ReadDir(dstSavePath)
	if err != nil {
		log.Printf("读取存档目录失败: %v", err)
		g.JSON(http.StatusOK, ServerListResponse{
			Status: 500,
			Data:   nil,
		})
		return
	}
	
	// 筛选有效存档
	var servers []ServerShortInfo
	for _, file := range files {
		if file.IsDir() {
			clusterPath := filepath.Join(dstSavePath, file.Name(), "cluster.ini")
			if _, err := os.Stat(clusterPath); !os.IsNotExist(err) {
				// 创建服务器信息
				server := ServerShortInfo{
					Name:     file.Name(),
					SavePath: filepath.Join(dstSavePath, file.Name()),
					Worlds:   []WorldInfo{},
				}
				
				// 获取世界列表
				worldFolders, err := ioutil.ReadDir(filepath.Join(dstSavePath, file.Name()))
				if err == nil {
					for _, worldFolder := range worldFolders {
						// 检查是否是目录，且不是管理文件
						if worldFolder.IsDir() && 
						   worldFolder.Name() != "backup" && 
						   !strings.HasSuffix(worldFolder.Name(), ".txt") &&
						   !strings.HasSuffix(worldFolder.Name(), ".ini") {
							
							// 尝试确定世界类型
							worldType := "unknown"
							levelDataPath := filepath.Join(dstSavePath, file.Name(), worldFolder.Name(), "leveldataoverride.lua")
							
							if data, err := ioutil.ReadFile(levelDataPath); err == nil {
								content := string(data)
								if strings.Contains(content, "location=\"forest\"") || strings.Contains(content, "\"location\"]=\"forest\"") {
									worldType = "forest"
								} else if strings.Contains(content, "location=\"cave\"") || strings.Contains(content, "\"location\"]=\"cave\"") {
									worldType = "cave"
								}
							}
							
							server.Worlds = append(server.Worlds, WorldInfo{
								Name: worldFolder.Name(),
								Type: worldType,
							})
						}
					}
				}
				
				servers = append(servers, server)
			}
		}
	}
	
	g.JSON(http.StatusOK, ServerListResponse{
		Status: 200,
		Data:   servers,
	})
}

// GetServerConfig 获取特定服务器的配置信息
func GetServerConfig(g *gin.Context) {
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, ServerDetailResponse{
			Status: 400,
			Data:   DSTServerConfig{},
		})
		return
	}
	
	// 构建cluster.ini路径
	clusterPath := filepath.Join(dstSavePath, saveName, "cluster.ini")
	
	// 检查文件是否存在
	if _, err := os.Stat(clusterPath); os.IsNotExist(err) {
		g.JSON(http.StatusOK, ServerDetailResponse{
			Status: 404,
			Data:   DSTServerConfig{},
		})
		return
	}
	
	// 读取并解析ini文件
	cfg, err := ini.Load(clusterPath)
	if err != nil {
		log.Printf("读取cluster.ini失败: %v", err)
		g.JSON(http.StatusOK, ServerDetailResponse{
			Status: 500,
			Data:   DSTServerConfig{},
		})
		return
	}
	
	// 解析配置到结构体
	serverConfig := DSTServerConfig{
		// STEAM部分
		SteamGroupID:      cfg.Section("STEAM").Key("steam_group_id").String(),
		SteamGroupAdmins:  cfg.Section("STEAM").Key("steam_group_admins").MustBool(false),
		SteamGroupOnly:    cfg.Section("STEAM").Key("steam_group_only").MustBool(false),
		
		// GAMEPLAY部分
		GameMode:        cfg.Section("GAMEPLAY").Key("game_mode").String(),
		PauseWhenEmpty:  cfg.Section("GAMEPLAY").Key("pause_when_empty").MustBool(true),
		VoteEnabled:     cfg.Section("GAMEPLAY").Key("vote_enabled").MustBool(true),
		PVP:             cfg.Section("GAMEPLAY").Key("pvp").MustBool(false),
		MaxPlayers:      cfg.Section("GAMEPLAY").Key("max_players").MustInt(6),
		
		// NETWORK部分
		ClusterName:        cfg.Section("NETWORK").Key("cluster_name").String(),
		ClusterDescription: cfg.Section("NETWORK").Key("cluster_description").String(),
		ClusterIntention:   cfg.Section("NETWORK").Key("cluster_intention").String(),
		ClusterLanguage:    cfg.Section("NETWORK").Key("cluster_language").String(),
		WhitelistSlots:     cfg.Section("NETWORK").Key("whitelist_slots").MustInt(0),
		IdleTimeout:        cfg.Section("NETWORK").Key("idle_timeout").MustInt(0),
		ClusterPassword:    cfg.Section("NETWORK").Key("cluster_password").String(),
		LanOnlyCluster:     cfg.Section("NETWORK").Key("lan_only_cluster").MustBool(false),
		OfflineCluster:     cfg.Section("NETWORK").Key("offline_cluster").MustBool(false),
		AutosaverEnabled:   cfg.Section("NETWORK").Key("autosaver_enabled").MustBool(true),
		TickRate:           cfg.Section("NETWORK").Key("tick_rate").MustInt(15),
		
		// MISC部分
		MaxSnapshots:   cfg.Section("MISC").Key("max_snapshots").MustInt(10),
		ConsoleEnabled: cfg.Section("MISC").Key("console_enabled").MustBool(true),
		
		// SHARD部分
		MasterIP:     cfg.Section("SHARD").Key("master_ip").String(),
		ShardEnabled: cfg.Section("SHARD").Key("shard_enabled").MustBool(true),
		BindIP:       cfg.Section("SHARD").Key("bind_ip").String(),
		MasterPort:   cfg.Section("SHARD").Key("master_port").MustInt(10888),
		ClusterKey:   cfg.Section("SHARD").Key("cluster_key").String(),
	}
	
	g.JSON(http.StatusOK, ServerDetailResponse{
		Status: 200,
		Data:   serverConfig,
	})
}

// UpdateServerConfig 更新服务器配置
func UpdateServerConfig(g *gin.Context) {
	// 请求参数结构
	type updateConfigRequest struct {
		SaveName string          `json:"savename" binding:"required"`
		Config   DSTServerConfig `json:"config" binding:"required"`
	}
	
	var req updateConfigRequest
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}
	
	saveName := req.SaveName
	config := req.Config
	
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}
	
	// 构建cluster.ini路径
	clusterPath := filepath.Join(dstSavePath, saveName, "cluster.ini")
	
	// 检查文件是否存在
	if _, err := os.Stat(clusterPath); os.IsNotExist(err) {
		g.JSON(http.StatusOK, gin.H{
			"status": 404,
			"msg":    "存档不存在",
		})
		return
	}
	
	// 读取现有ini文件
	cfg, err := ini.Load(clusterPath)
	if err != nil {
		log.Printf("读取cluster.ini失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "读取配置文件失败",
		})
		return
	}
	
	// 更新STEAM部分
	steamSection := cfg.Section("STEAM")
	steamSection.Key("steam_group_id").SetValue(config.SteamGroupID)
	steamSection.Key("steam_group_admins").SetValue(fmt.Sprintf("%t", config.SteamGroupAdmins))
	steamSection.Key("steam_group_only").SetValue(fmt.Sprintf("%t", config.SteamGroupOnly))
	
	// 更新GAMEPLAY部分
	gameplaySection := cfg.Section("GAMEPLAY")
	gameplaySection.Key("game_mode").SetValue(config.GameMode)
	gameplaySection.Key("pause_when_empty").SetValue(fmt.Sprintf("%t", config.PauseWhenEmpty))
	gameplaySection.Key("vote_enabled").SetValue(fmt.Sprintf("%t", config.VoteEnabled))
	gameplaySection.Key("pvp").SetValue(fmt.Sprintf("%t", config.PVP))
	gameplaySection.Key("max_players").SetValue(fmt.Sprintf("%d", config.MaxPlayers))
	
	// 更新NETWORK部分
	networkSection := cfg.Section("NETWORK")
	networkSection.Key("cluster_name").SetValue(config.ClusterName)
	networkSection.Key("cluster_description").SetValue(config.ClusterDescription)
	networkSection.Key("cluster_intention").SetValue(config.ClusterIntention)
	networkSection.Key("cluster_language").SetValue(config.ClusterLanguage)
	networkSection.Key("whitelist_slots").SetValue(fmt.Sprintf("%d", config.WhitelistSlots))
	networkSection.Key("idle_timeout").SetValue(fmt.Sprintf("%d", config.IdleTimeout))
	networkSection.Key("cluster_password").SetValue(config.ClusterPassword)
	networkSection.Key("lan_only_cluster").SetValue(fmt.Sprintf("%t", config.LanOnlyCluster))
	networkSection.Key("offline_cluster").SetValue(fmt.Sprintf("%t", config.OfflineCluster))
	networkSection.Key("autosaver_enabled").SetValue(fmt.Sprintf("%t", config.AutosaverEnabled))
	networkSection.Key("tick_rate").SetValue(fmt.Sprintf("%d", config.TickRate))
	
	// 更新MISC部分
	miscSection := cfg.Section("MISC")
	miscSection.Key("max_snapshots").SetValue(fmt.Sprintf("%d", config.MaxSnapshots))
	miscSection.Key("console_enabled").SetValue(fmt.Sprintf("%t", config.ConsoleEnabled))
	
	// 更新SHARD部分
	shardSection := cfg.Section("SHARD")
	shardSection.Key("master_ip").SetValue(config.MasterIP)
	shardSection.Key("shard_enabled").SetValue(fmt.Sprintf("%t", config.ShardEnabled))
	shardSection.Key("bind_ip").SetValue(config.BindIP)
	shardSection.Key("master_port").SetValue(fmt.Sprintf("%d", config.MasterPort))
	shardSection.Key("cluster_key").SetValue(config.ClusterKey)
	
	// 保存修改后的配置
	if err := cfg.SaveTo(clusterPath); err != nil {
		log.Printf("保存cluster.ini失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "保存配置文件失败",
		})
		return
	}
	
	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新配置成功",
	})
}

// GetAdminList 获取管理员列表
func GetAdminList(g *gin.Context) {
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, ListResponse{
			Status: 400,
			Data:   nil,
		})
		return
	}
	
	// 构建管理员列表文件路径
	adminListPath := filepath.Join(dstSavePath, saveName, "adminlist.txt")
	
	// 检查文件是否存在
	if _, err := os.Stat(adminListPath); os.IsNotExist(err) {
		// 如果文件不存在，返回空列表
		g.JSON(http.StatusOK, ListResponse{
			Status: 200,
			Data:   []string{},
		})
		return
	}
	
	// 读取管理员列表文件
	content, err := ioutil.ReadFile(adminListPath)
	if err != nil {
		log.Printf("读取管理员列表失败: %v", err)
		g.JSON(http.StatusOK, ListResponse{
			Status: 500,
			Data:   nil,
		})
		return
	}
	
	// 解析管理员列表，过滤空行
	adminList := []string{}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			adminList = append(adminList, line)
		}
	}
	
	g.JSON(http.StatusOK, ListResponse{
		Status: 200,
		Data:   adminList,
	})
}

// UpdateAdminList 更新管理员列表
func UpdateAdminList(g *gin.Context) {
	// 请求参数结构
	type updateListRequest struct {
		SaveName string   `json:"savename" binding:"required"`
		List     []string `json:"list"`
	}
	
	var req updateListRequest
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}
	
	saveName := req.SaveName
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}
	
	// 构建管理员列表文件路径
	adminListPath := filepath.Join(dstSavePath, saveName, "adminlist.txt")
	
	// 确保目录存在
	saveDir := filepath.Join(dstSavePath, saveName)
	if _, err := os.Stat(saveDir); os.IsNotExist(err) {
		g.JSON(http.StatusOK, gin.H{
			"status": 404,
			"msg":    "存档不存在",
		})
		return
	}
	
	// 将列表转换为带换行符的字符串
	content := strings.Join(req.List, "\n")
	if content != "" {
		content += "\n" // 确保最后有换行符
	}
	
	// 写入管理员列表文件
	if err := ioutil.WriteFile(adminListPath, []byte(content), 0644); err != nil {
		log.Printf("保存管理员列表失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "保存管理员列表失败",
		})
		return
	}
	
	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新管理员列表成功",
	})
}

// GetBlockList 获取黑名单列表
func GetBlockList(g *gin.Context) {
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, ListResponse{
			Status: 400,
			Data:   nil,
		})
		return
	}
	
	// 构建黑名单文件路径
	blockListPath := filepath.Join(dstSavePath, saveName, "blocklist.txt")
	
	// 检查文件是否存在
	if _, err := os.Stat(blockListPath); os.IsNotExist(err) {
		// 如果文件不存在，返回空列表
		g.JSON(http.StatusOK, ListResponse{
			Status: 200,
			Data:   []string{},
		})
		return
	}
	
	// 读取黑名单文件
	content, err := ioutil.ReadFile(blockListPath)
	if err != nil {
		log.Printf("读取黑名单失败: %v", err)
		g.JSON(http.StatusOK, ListResponse{
			Status: 500,
			Data:   nil,
		})
		return
	}
	
	// 解析黑名单列表，过滤空行
	blockList := []string{}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			blockList = append(blockList, line)
		}
	}
	
	g.JSON(http.StatusOK, ListResponse{
		Status: 200,
		Data:   blockList,
	})
}

// UpdateBlockList 更新黑名单列表
func UpdateBlockList(g *gin.Context) {
	// 请求参数结构
	type updateListRequest struct {
		SaveName string   `json:"savename" binding:"required"`
		List     []string `json:"list"`
	}
	
	var req updateListRequest
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}
	
	saveName := req.SaveName
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}
	
	// 构建黑名单文件路径
	blockListPath := filepath.Join(dstSavePath, saveName, "blocklist.txt")
	
	// 确保目录存在
	saveDir := filepath.Join(dstSavePath, saveName)
	if _, err := os.Stat(saveDir); os.IsNotExist(err) {
		g.JSON(http.StatusOK, gin.H{
			"status": 404,
			"msg":    "存档不存在",
		})
		return
	}
	
	// 将列表转换为带换行符的字符串
	content := strings.Join(req.List, "\n")
	if content != "" {
		content += "\n" // 确保最后有换行符
	}
	
	// 写入黑名单文件
	if err := ioutil.WriteFile(blockListPath, []byte(content), 0644); err != nil {
		log.Printf("保存黑名单失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "保存黑名单失败",
		})
		return
	}
	
	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新黑名单成功",
	})
}

// GetWhiteList 获取白名单列表
func GetWhiteList(g *gin.Context) {
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, ListResponse{
			Status: 400,
			Data:   nil,
		})
		return
	}
	
	// 构建白名单文件路径
	whiteListPath := filepath.Join(dstSavePath, saveName, "whitelist.txt")
	
	// 检查文件是否存在
	if _, err := os.Stat(whiteListPath); os.IsNotExist(err) {
		// 如果文件不存在，返回空列表
		g.JSON(http.StatusOK, ListResponse{
			Status: 200,
			Data:   []string{},
		})
		return
	}
	
	// 读取白名单文件
	content, err := ioutil.ReadFile(whiteListPath)
	if err != nil {
		log.Printf("读取白名单失败: %v", err)
		g.JSON(http.StatusOK, ListResponse{
			Status: 500,
			Data:   nil,
		})
		return
	}
	
	// 解析白名单列表，过滤空行
	whiteList := []string{}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			whiteList = append(whiteList, line)
		}
	}
	
	g.JSON(http.StatusOK, ListResponse{
		Status: 200,
		Data:   whiteList,
	})
}

// UpdateWhiteList 更新白名单列表
func UpdateWhiteList(g *gin.Context) {
	// 请求参数结构
	type updateListRequest struct {
		SaveName string   `json:"savename" binding:"required"`
		List     []string `json:"list"`
	}
	
	var req updateListRequest
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}
	
	saveName := req.SaveName
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}
	
	// 构建白名单文件路径
	whiteListPath := filepath.Join(dstSavePath, saveName, "whitelist.txt")
	
	// 确保目录存在
	saveDir := filepath.Join(dstSavePath, saveName)
	if _, err := os.Stat(saveDir); os.IsNotExist(err) {
		g.JSON(http.StatusOK, gin.H{
			"status": 404,
			"msg":    "存档不存在",
		})
		return
	}
	
	// 将列表转换为带换行符的字符串
	content := strings.Join(req.List, "\n")
	if content != "" {
		content += "\n" // 确保最后有换行符
	}
	
	// 写入白名单文件
	if err := ioutil.WriteFile(whiteListPath, []byte(content), 0644); err != nil {
		log.Printf("保存白名单失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "保存白名单失败",
		})
		return
	}
	
	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新白名单成功",
	})
}

// GetClusterToken 获取服务器令牌
func GetClusterToken(g *gin.Context) {
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, TokenResponse{
			Status: 400,
			Data:   "",
		})
		return
	}
	
	// 构建令牌文件路径
	tokenPath := filepath.Join(dstSavePath, saveName, "cluster_token.txt")
	
	// 检查文件是否存在
	if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
		// 如果文件不存在，返回空字符串
		g.JSON(http.StatusOK, TokenResponse{
			Status: 200,
			Data:   "",
		})
		return
	}
	
	// 读取令牌文件
	content, err := ioutil.ReadFile(tokenPath)
	if err != nil {
		log.Printf("读取服务器令牌失败: %v", err)
		g.JSON(http.StatusOK, TokenResponse{
			Status: 500,
			Data:   "",
		})
		return
	}
	
	// 返回去除空白字符的令牌
	token := strings.TrimSpace(string(content))
	
	g.JSON(http.StatusOK, TokenResponse{
		Status: 200,
		Data:   token,
	})
}

// UpdateClusterToken 更新服务器令牌
func UpdateClusterToken(g *gin.Context) {
	// 请求参数结构
	type updateTokenRequest struct {
		SaveName string `json:"savename" binding:"required"`
		Token    string `json:"token"`
	}
	
	var req updateTokenRequest
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}
	
	saveName := req.SaveName
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}
	
	// 构建令牌文件路径
	tokenPath := filepath.Join(dstSavePath, saveName, "cluster_token.txt")
	
	// 确保目录存在
	saveDir := filepath.Join(dstSavePath, saveName)
	if _, err := os.Stat(saveDir); os.IsNotExist(err) {
		g.JSON(http.StatusOK, gin.H{
			"status": 404,
			"msg":    "存档不存在",
		})
		return
	}
	
	// 写入令牌文件
	token := strings.TrimSpace(req.Token)
	if err := ioutil.WriteFile(tokenPath, []byte(token), 0644); err != nil {
		log.Printf("保存服务器令牌失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "保存服务器令牌失败",
		})
		return
	}
	
	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新服务器令牌成功",
	})
} 