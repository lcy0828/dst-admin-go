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

// 服务器列表响应
type ServerListResponse struct {
	Status int               `json:"status"`
	Data   []ServerShortInfo `json:"data"`
}

// 服务器简要信息
type ServerShortInfo struct {
	Name     string `json:"name"`     // 存档名称
	SavePath string `json:"savepath"` // 存档路径
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
				servers = append(servers, ServerShortInfo{
					Name:     file.Name(),
					SavePath: filepath.Join(dstSavePath, file.Name()),
				})
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
	saveName := g.Param("savename")
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
	saveName := g.Param("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}
	
	// 服务器配置参数
	var config DSTServerConfig
	if err := g.BindJSON(&config); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
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