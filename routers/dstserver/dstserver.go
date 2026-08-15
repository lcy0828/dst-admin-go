package dstserver

import (
	"dont/pkg/configpath"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 配置变量，加载时从配置文件初始化
var (
	dstSavePath string // DST存档目录
)

// 初始化函数，从配置文件读取配置
func init() {
	// 默认配置
	dstSavePath = "./Klei/DoNotStarveTogether"

	// 尝试从配置文件读取
	configFile := configpath.Current()
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
				dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
				log.Printf("从配置文件加载DST存档路径: %s", dstSavePath)
			}
		}
	} else {
		log.Printf("配置文件不存在，使用默认DST存档路径: %s", dstSavePath)
	}
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
	Name     string      `json:"name"`     // 存档名称
	SavePath string      `json:"savepath"` // 存档路径
	Worlds   []WorldInfo `json:"worlds"`   // 世界列表
}

// 世界信息
type WorldInfo struct {
	Name     string `json:"name"`      // 世界名称
	Type     string `json:"type"`      // 世界类型 (forest/cave)
	IsMaster bool   `json:"is_master"` // 是否为主世界
}

// ClusterConfigResponse 集群配置响应结构
type ClusterConfigResponse struct {
	Status int                          `json:"status"`
	Data   map[string]map[string]string `json:"data"`
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

							// 检查是否为主世界
							isMaster := false
							serverIniPath := filepath.Join(dstSavePath, file.Name(), worldFolder.Name(), "server.ini")
							if _, err := os.Stat(serverIniPath); !os.IsNotExist(err) {
								// 读取并解析server.ini文件
								if cfg, err := ini.Load(serverIniPath); err == nil {
									// 检查是否有SHARD部分和is_master配置项
									if cfg.Section("SHARD").HasKey("is_master") {
										// 获取is_master的值并转换为布尔值
										isMasterStr := cfg.Section("SHARD").Key("is_master").String()
										isMaster = strings.ToLower(isMasterStr) == "true"
									}
								}
							}

							server.Worlds = append(server.Worlds, WorldInfo{
								Name:     worldFolder.Name(),
								Type:     worldType,
								IsMaster: isMaster,
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

// GetClusterConfig 获取特定服务器的 cluster.ini 配置信息
func GetClusterConfig(g *gin.Context) {
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, ClusterConfigResponse{
			Status: 400,
			Data:   nil,
		})
		return
	}

	// 构建cluster.ini路径
	clusterPath := filepath.Join(dstSavePath, saveName, "cluster.ini")

	// 检查文件是否存在
	if _, err := os.Stat(clusterPath); os.IsNotExist(err) {
		g.JSON(http.StatusOK, ClusterConfigResponse{
			Status: 404,
			Data:   nil,
		})
		return
	}

	// 读取并解析ini文件
	cfg, err := ini.Load(clusterPath)
	if err != nil {
		log.Printf("读取cluster.ini失败: %v", err)
		g.JSON(http.StatusOK, ClusterConfigResponse{
			Status: 500,
			Data:   nil,
		})
		return
	}

	// 将所有配置项转换为map
	configMap := make(map[string]map[string]string)

	// 遍历所有section
	for _, section := range cfg.Sections() {
		sectionName := section.Name()

		// 跳过DEFAULT section
		if sectionName == "DEFAULT" {
			continue
		}

		// 创建section map
		configMap[sectionName] = make(map[string]string)

		// 遍历section中的所有key
		for _, key := range section.Keys() {
			configMap[sectionName][key.Name()] = key.String()
		}
	}

	g.JSON(http.StatusOK, ClusterConfigResponse{
		Status: 200,
		Data:   configMap,
	})
}

// UpdateClusterConfig 更新服务器的cluster.ini配置
func UpdateClusterConfig(g *gin.Context) {
	// 请求参数结构
	type updateClusterConfigRequest struct {
		SaveName string                       `json:"savename" binding:"required"`
		Config   map[string]map[string]string `json:"config" binding:"required"`
	}

	var req updateClusterConfigRequest
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
	saveDir := filepath.Join(dstSavePath, saveName)
	clusterPath := filepath.Join(saveDir, "cluster.ini")

	var cfg *ini.File
	var err error

	// 检查文件是否存在
	if _, fileErr := os.Stat(clusterPath); os.IsNotExist(fileErr) {
		// 检查存档目录是否存在，如果不存在则创建
		if _, dirErr := os.Stat(saveDir); os.IsNotExist(dirErr) {
			if err := os.MkdirAll(saveDir, 0755); err != nil {
				log.Printf("创建存档目录失败: %v", err)
				g.JSON(http.StatusOK, gin.H{
					"status": 500,
					"msg":    "创建存档目录失败",
				})
				return
			}
		}

		// 创建新的ini文件
		cfg = ini.Empty()
	} else {
		// 读取现有ini文件
		cfg, err = ini.Load(clusterPath)
		if err != nil {
			log.Printf("读取cluster.ini失败: %v", err)
			g.JSON(http.StatusOK, gin.H{
				"status": 500,
				"msg":    "读取配置文件失败",
			})
			return
		}
	}

	// 更新配置
	for sectionName, sectionConfig := range config {
		section := cfg.Section(sectionName)

		for key, value := range sectionConfig {
			section.Key(key).SetValue(value)
		}
	}

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
	// 读取请求体
	var req struct {
		SaveName string   `json:"savename" binding:"required"`
		List     []string `json:"list" binding:"required"`
	}
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}

	if req.SaveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}

	// 构建管理员列表文件路径
	adminListPath := filepath.Join(dstSavePath, req.SaveName, "adminlist.txt")

	// 将管理员列表写入文件
	content := strings.Join(req.List, "\n")
	if err := ioutil.WriteFile(adminListPath, []byte(content), 0644); err != nil {
		log.Printf("写入管理员列表失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "写入管理员列表失败",
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

	// 构建黑名单列表文件路径
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

	// 读取黑名单列表文件
	content, err := ioutil.ReadFile(blockListPath)
	if err != nil {
		log.Printf("读取黑名单列表失败: %v", err)
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
	// 读取请求体
	var req struct {
		SaveName string   `json:"savename" binding:"required"`
		List     []string `json:"list" binding:"required"`
	}
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}

	if req.SaveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}

	// 构建黑名单列表文件路径
	blockListPath := filepath.Join(dstSavePath, req.SaveName, "blocklist.txt")

	// 将黑名单列表写入文件
	content := strings.Join(req.List, "\n")
	if err := ioutil.WriteFile(blockListPath, []byte(content), 0644); err != nil {
		log.Printf("写入黑名单列表失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "写入黑名单列表失败",
		})
		return
	}

	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新黑名单列表成功",
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

	// 构建白名单列表文件路径
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

	// 读取白名单列表文件
	content, err := ioutil.ReadFile(whiteListPath)
	if err != nil {
		log.Printf("读取白名单列表失败: %v", err)
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
	// 读取请求体
	var req struct {
		SaveName string   `json:"savename" binding:"required"`
		List     []string `json:"list" binding:"required"`
	}
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}

	if req.SaveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}

	// 构建白名单列表文件路径
	whiteListPath := filepath.Join(dstSavePath, req.SaveName, "whitelist.txt")

	// 将白名单列表写入文件
	content := strings.Join(req.List, "\n")
	if err := ioutil.WriteFile(whiteListPath, []byte(content), 0644); err != nil {
		log.Printf("写入白名单列表失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "写入白名单列表失败",
		})
		return
	}

	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新白名单列表成功",
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

	// 构建cluster_token.txt路径
	tokenPath := filepath.Join(dstSavePath, saveName, "cluster_token.txt")

	// 检查文件是否存在
	if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
		g.JSON(http.StatusOK, TokenResponse{
			Status: 404,
			Data:   "",
		})
		return
	}

	// 读取服务器令牌
	token, err := ioutil.ReadFile(tokenPath)
	if err != nil {
		log.Printf("读取服务器令牌失败: %v", err)
		g.JSON(http.StatusOK, TokenResponse{
			Status: 500,
			Data:   "",
		})
		return
	}

	g.JSON(http.StatusOK, TokenResponse{
		Status: 200,
		Data:   string(token),
	})
}

// UpdateClusterToken 更新服务器令牌
func UpdateClusterToken(g *gin.Context) {
	// 读取请求体
	var req struct {
		SaveName string `json:"savename" binding:"required"`
		Token    string `json:"token" binding:"required"`
	}
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}

	if req.SaveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}

	// 构建cluster_token.txt路径
	tokenPath := filepath.Join(dstSavePath, req.SaveName, "cluster_token.txt")

	// 将服务器令牌写入文件
	if err := ioutil.WriteFile(tokenPath, []byte(req.Token), 0644); err != nil {
		log.Printf("写入服务器令牌失败: %v", err)
		g.JSON(http.StatusOK, gin.H{
			"status": 500,
			"msg":    "写入服务器令牌失败",
		})
		return
	}

	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新服务器令牌成功",
	})
}

// WorldOverridesResponse 世界配置响应结构
type WorldOverridesResponse struct {
	Status int                    `json:"status"`
	Data   map[string]interface{} `json:"data"`
	Msg    string                 `json:"msg,omitempty"`
}

// ServerIniConfig 服务器配置结构
type ServerIniConfig struct {
	Network struct {
		ServerPort int `ini:"server_port" json:"server_port"`
	} `ini:"NETWORK" json:"network"`

	Shard struct {
		IsMaster bool   `ini:"is_master" json:"is_master"`
		Name     string `ini:"name" json:"name"`
		ID       int    `ini:"id" json:"id"`
	} `ini:"SHARD" json:"shard"`

	Account struct {
		EncodeUserPath bool `ini:"encode_user_path" json:"encode_user_path"`
	} `ini:"ACCOUNT" json:"account"`

	Steam struct {
		MasterServerPort   int `ini:"master_server_port" json:"master_server_port"`
		AuthenticationPort int `ini:"authentication_port" json:"authentication_port"`
	} `ini:"STEAM" json:"steam"`
}

// ServerIniResponse 服务器配置响应结构
type ServerIniResponse struct {
	Status int             `json:"status"`
	Data   ServerIniConfig `json:"data"`
	Msg    string          `json:"msg,omitempty"`
}

// UpdateServerIniRequest 更新服务器配置请求结构
type UpdateServerIniRequest struct {
	SaveName  string          `json:"savename" binding:"required"`
	WorldName string          `json:"worldname" binding:"required"`
	Config    ServerIniConfig `json:"config" binding:"required"`
}

// UpdateWorldOverridesRequest 更新世界配置请求结构
type UpdateWorldOverridesRequest struct {
	SaveName  string                 `json:"savename" binding:"required"`
	WorldName string                 `json:"worldname" binding:"required"`
	Overrides map[string]interface{} `json:"overrides" binding:"required"`
}

// CreateForestWorldRequest 创建森林世界配置请求结构
type CreateForestWorldRequest struct {
	SaveName  string                 `json:"savename" binding:"required"`
	WorldName string                 `json:"worldname" binding:"required"`
	Overrides map[string]interface{} `json:"overrides" binding:"required"`
}

// CreateCaveWorldRequest 创建洞穴世界配置请求结构
type CreateCaveWorldRequest struct {
	SaveName  string                 `json:"savename" binding:"required"`
	WorldName string                 `json:"worldname" binding:"required"`
	Overrides map[string]interface{} `json:"overrides" binding:"required"`
}

// TemplateWorldRequest 从模板创建世界配置请求结构
type TemplateWorldRequest struct {
	SaveName  string `json:"savename" binding:"required"`
	WorldName string `json:"worldname" binding:"required"`
}

// GetWorldOverrides 获取指定房间和世界的leveldataoverride.lua中的overrides字段
func GetWorldOverrides(g *gin.Context) {
	// 获取请求参数
	saveName := g.Query("savename")
	worldName := g.Query("worldname")

	// 记录请求参数
	log.Printf("接收到获取世界配置请求 - 存档名称: %s, 世界名称: %s", saveName, worldName)

	if saveName == "" || worldName == "" {
		errorMsg := "缺少必要的参数"
		if saveName == "" {
			errorMsg += ": savename"
		}
		if worldName == "" {
			if saveName == "" {
				errorMsg += " 和 worldname"
			} else {
				errorMsg += ": worldname"
			}
		}
		log.Printf("请求参数错误: %s", errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 400,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	// 构建leveldataoverride.lua文件路径
	levelDataPath := filepath.Join(dstSavePath, saveName, worldName, "leveldataoverride.lua")
	log.Printf("尝试读取文件: %s", levelDataPath)

	// 检查文件是否存在
	if _, err := os.Stat(levelDataPath); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("文件不存在: %s", levelDataPath)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 404,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	// 读取leveldataoverride.lua文件内容
	content, err := ioutil.ReadFile(levelDataPath)
	if err != nil {
		errorMsg := fmt.Sprintf("读取leveldataoverride.lua失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 500,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	// 解析Lua文件内容，提取overrides字段
	overrides, err := parseLevelDataOverrides(string(content))
	if err != nil {
		errorMsg := fmt.Sprintf("解析leveldataoverride.lua失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 500,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	// 检查是否成功提取到overrides字段
	if len(overrides) == 0 {
		log.Print("成功解析文件，但没有找到overrides字段或字段为空")
	}

	log.Printf("成功获取世界配置 - 存档: %s, 世界: %s, 配置项数量: %d", saveName, worldName, len(overrides))
	g.JSON(http.StatusOK, WorldOverridesResponse{
		Status: 200,
		Data:   overrides,
	})
}

// parseLevelDataOverrides 解析leveldataoverride.lua文件中的overrides字段
func parseLevelDataOverrides(content string) (map[string]interface{}, error) {
	// 创建一个空的map来存储结果
	result := make(map[string]interface{})

	// 使用正则表达式匹配overrides部分
	// 注意：我们使用(?s)模式使.(点)可以匹配换行符
	overridesPattern := regexp.MustCompile(`(?s)overrides\s*=\s*\{(.+?)\},`)
	matches := overridesPattern.FindStringSubmatch(content)

	if len(matches) < 2 {
		// 尝试另一种模式，如果overrides是最后一个字段
		overridesPattern = regexp.MustCompile(`(?s)overrides\s*=\s*\{(.+?)\}\s*,?\s*$`)
		matches = overridesPattern.FindStringSubmatch(content)
		if len(matches) < 2 {
			// 尝试第三种模式，匹配中间的overrides
			overridesPattern = regexp.MustCompile(`(?s)overrides\s*=\s*\{(.+?)\}\s*,\s*["\w]`)
			matches = overridesPattern.FindStringSubmatch(content)
			if len(matches) < 2 {
				// 最后尝试一种更宽松的模式
				overridesPattern = regexp.MustCompile(`(?s)overrides\s*=\s*\{(.+?)\}`)
				matches = overridesPattern.FindStringSubmatch(content)
				if len(matches) < 2 {
					// 没有找到overrides部分
					log.Print("没有找到overrides部分")
					return result, nil
				}
			}
		}
	}

	// 提取overrides内容
	overridesContent := matches[1]
	log.Printf("成功提取overrides内容，长度: %d字节", len(overridesContent))

	// 解析键值对
	// 匹配形如 ["key"]="value" 或 key="value" 或 key=value 的模式
	pairs := regexp.MustCompile(`\[?"?([\w]+)"?\]?\s*=\s*"?([^,"\}\n]+)"?\s*,?`)
	allMatches := pairs.FindAllStringSubmatch(overridesContent, -1)

	for _, match := range allMatches {
		if len(match) >= 3 {
			key := strings.TrimSpace(match[1])
			value := strings.TrimSpace(match[2])
			result[key] = value
		}
	}

	return result, nil
}

// UpdateWorldOverrides 更新指定房间和世界的leveldataoverride.lua中的overrides字段
func UpdateWorldOverrides(g *gin.Context) {
	// 解析请求体
	var req UpdateWorldOverridesRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		log.Printf("解析请求体失败: %v", err)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 400,
			Data:   nil,
			Msg:    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 记录请求参数
	log.Printf("接收到更新世界配置请求 - 存档名称: %s, 世界名称: %s, 配置项数量: %d",
		req.SaveName, req.WorldName, len(req.Overrides))

	// 构建leveldataoverride.lua文件路径
	levelDataPath := filepath.Join(dstSavePath, req.SaveName, req.WorldName, "leveldataoverride.lua")
	log.Printf("尝试读取文件: %s", levelDataPath)

	// 检查文件是否存在
	if _, err := os.Stat(levelDataPath); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("文件不存在: %s", levelDataPath)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 404,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	// 读取leveldataoverride.lua文件内容
	content, err := ioutil.ReadFile(levelDataPath)
	if err != nil {
		errorMsg := fmt.Sprintf("读取leveldataoverride.lua失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 500,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	// 将文件内容转换为字符串
	fileContent := string(content)

	// 使用正则表达式匹配overrides部分
	// 注意：我们使用(?s)模式使.(点)可以匹配换行符
	overridesPattern := regexp.MustCompile(`(?s)(overrides\s*=\s*\{)(.+?)(\}),`)
	matches := overridesPattern.FindStringSubmatch(fileContent)

	if len(matches) < 4 {
		// 尝试另一种模式，如果overrides是最后一个字段
		overridesPattern = regexp.MustCompile(`(?s)(overrides\s*=\s*\{)(.+?)(\})\s*,?\s*$`)
		matches = overridesPattern.FindStringSubmatch(fileContent)
		if len(matches) < 4 {
			// 尝试第三种模式，匹配中间的overrides
			overridesPattern = regexp.MustCompile(`(?s)(overrides\s*=\s*\{)(.+?)(\})\s*,\s*["\w]`)
			matches = overridesPattern.FindStringSubmatch(fileContent)
			if len(matches) < 4 {
				// 最后尝试一种更宽松的模式
				overridesPattern = regexp.MustCompile(`(?s)(overrides\s*=\s*\{)(.+?)(\})`)
				matches = overridesPattern.FindStringSubmatch(fileContent)
				if len(matches) < 4 {
					errorMsg := "无法在文件中找到overrides部分"
					log.Print(errorMsg)
					g.JSON(http.StatusOK, WorldOverridesResponse{
						Status: 500,
						Data:   nil,
						Msg:    errorMsg,
					})
					return
				}
			}
		}
	}

	// 构建新的overrides内容
	newOverridesContent := "\n"
	for key, value := range req.Overrides {
		// 将值转换为字符串
		valueStr := fmt.Sprintf("%v", value)
		// 检查值的类型，数字、true、false不需要引号，其他字符串需要引号
		_, errFloat := strconv.ParseFloat(valueStr, 64)
		if valueStr != "true" && valueStr != "false" && errFloat != nil {
			valueStr = "\"" + valueStr + "\""
		}
		newOverridesContent += fmt.Sprintf("    %s=%s,\n", key, valueStr)
	}

	// 替换文件中的overrides部分
	newContent := overridesPattern.ReplaceAllString(fileContent, "${1}"+newOverridesContent+"${3}")

	// 直接覆盖原文件，不进行备份
	log.Printf("将直接覆盖原文件: %s", levelDataPath)

	// 写入新文件
	if err := ioutil.WriteFile(levelDataPath, []byte(newContent), 0644); err != nil {
		errorMsg := fmt.Sprintf("写入文件失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 500,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	log.Printf("成功更新世界配置 - 存档: %s, 世界: %s, 配置项数量: %d", req.SaveName, req.WorldName, len(req.Overrides))

	// 返回更新后的配置
	g.JSON(http.StatusOK, WorldOverridesResponse{
		Status: 200,
		Data:   req.Overrides,
		Msg:    "更新世界配置成功",
	})
}

// CreateForestWorld 创建或更新森林世界的leveldataoverride.lua文件
func CreateForestWorld(g *gin.Context) {
	// 解析请求体
	var req CreateForestWorldRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		log.Printf("解析请求体失败: %v", err)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 400,
			Data:   nil,
			Msg:    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 记录请求参数
	log.Printf("接收到创建或更新森林世界请求 - 存档名称: %s, 世界名称: %s, 配置项数量: %d",
		req.SaveName, req.WorldName, len(req.Overrides))

	// 构建leveldataoverride.lua文件路径
	levelDataPath := filepath.Join(dstSavePath, req.SaveName, req.WorldName, "leveldataoverride.lua")
	log.Printf("尝试创建文件: %s", levelDataPath)

	// 检查目录是否存在，如果不存在则创建
	dirPath := filepath.Join(dstSavePath, req.SaveName, req.WorldName)
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			errorMsg := fmt.Sprintf("创建目录失败: %v", err)
			log.Print(errorMsg)
			g.JSON(http.StatusOK, WorldOverridesResponse{
				Status: 500,
				Data:   nil,
				Msg:    errorMsg,
			})
			return
		}
		log.Printf("成功创建目录: %s", dirPath)
	}

	// 检查文件是否已存在
	if _, err := os.Stat(levelDataPath); err == nil {
		// 文件已存在，直接覆盖
		log.Printf("文件已存在，将直接覆盖: %s", levelDataPath)
	}

	// 构建新的overrides内容
	overridesContent := "\n"

	// 将键排序
	keys := make([]string, 0, len(req.Overrides))
	for key := range req.Overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// 按排序后的键生成内容
	for _, key := range keys {
		value := req.Overrides[key]
		// 将值转换为字符串
		valueStr := fmt.Sprintf("%v", value)
		// 检查值的类型，数字、true、false不需要引号，其他字符串需要引号
		_, errFloat := strconv.ParseFloat(valueStr, 64)
		if valueStr != "true" && valueStr != "false" && errFloat != nil {
			valueStr = "\"" + valueStr + "\""
		}
		overridesContent += fmt.Sprintf("    %s=%s,\n", key, valueStr)
	}

	// 在overrides最后增加指定的配置项
	overridesContent += "    has_ocean=true,\n"
	overridesContent += "    keep_disconnected_tiles=true,\n"
	overridesContent += "    layout_mode=\"LinkNodesByKeys\",\n"
	overridesContent += "    no_joining_islands=true,\n"
	overridesContent += "    no_wormholes_to_disconnected_tiles=true,\n"
	overridesContent += "    wormhole_prefab=\"wormhole\",\n"

	// 构建森林世界模板
	template := fmt.Sprintf(`return {
  desc="由https://github.com/lcy0828/dst-admin-go面板生成的世界",
  hideminimap=false,
  id="SURVIVAL_TOGETHER",
  location="forest",
  max_playlist_position=999,
  min_playlist_position=0,
  name="游山玩水——基于dst-admin-go可视化面板一键部署创建！",
  numrandom_set_pieces=4,
  override_level_string=false,
  overrides={%s  },
  playstyle="survival",
  random_set_pieces={
    "Sculptures_2",
    "Sculptures_3",
    "Sculptures_4",
    "Sculptures_5",
    "Chessy_1",
    "Chessy_2",
    "Chessy_3",
    "Chessy_4",
    "Chessy_5",
    "Chessy_6",
    "Maxwell1",
    "Maxwell2",
    "Maxwell3",
    "Maxwell4",
    "Maxwell6",
    "Maxwell7",
    "Warzone_1",
    "Warzone_2",
    "Warzone_3"
  },
  required_prefabs={ "multiplayer_portal" },
  required_setpieces={ "Sculptures_1", "Maxwell5" },
  settings_desc="标准《饥荒》体验。",
  settings_id="SURVIVAL_TOGETHER",
  settings_name="生存",
  substitutes={  },
  version=4,
  worldgen_desc="标准《饥荒》体验。",
  worldgen_id="SURVIVAL_TOGETHER",
  worldgen_name="生存"
}
`, overridesContent)

	// 写入新文件
	if err := ioutil.WriteFile(levelDataPath, []byte(template), 0644); err != nil {
		errorMsg := fmt.Sprintf("写入文件失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 500,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	log.Printf("成功创建或更新森林世界配置 - 存档: %s, 世界: %s, 配置项数量: %d", req.SaveName, req.WorldName, len(req.Overrides))

	// 返回创建或更新后的配置
	g.JSON(http.StatusOK, WorldOverridesResponse{
		Status: 200,
		Data:   req.Overrides,
		Msg:    "创建或更新森林世界配置成功",
	})
}

// CreateCaveWorld 创建或更新洞穴世界的leveldataoverride.lua文件
func CreateCaveWorld(g *gin.Context) {
	// 解析请求体
	var req CreateCaveWorldRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		log.Printf("解析请求体失败: %v", err)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 400,
			Data:   nil,
			Msg:    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 记录请求参数
	log.Printf("接收到创建或更新洞穴世界请求 - 存档名称: %s, 世界名称: %s, 配置项数量: %d",
		req.SaveName, req.WorldName, len(req.Overrides))

	// 构建leveldataoverride.lua文件路径
	levelDataPath := filepath.Join(dstSavePath, req.SaveName, req.WorldName, "leveldataoverride.lua")
	log.Printf("尝试创建文件: %s", levelDataPath)

	// 检查目录是否存在，如果不存在则创建
	dirPath := filepath.Join(dstSavePath, req.SaveName, req.WorldName)
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			errorMsg := fmt.Sprintf("创建目录失败: %v", err)
			log.Print(errorMsg)
			g.JSON(http.StatusOK, WorldOverridesResponse{
				Status: 500,
				Data:   nil,
				Msg:    errorMsg,
			})
			return
		}
		log.Printf("成功创建目录: %s", dirPath)
	}

	// 检查文件是否已存在
	if _, err := os.Stat(levelDataPath); err == nil {
		// 文件已存在，直接覆盖
		log.Printf("文件已存在，将直接覆盖: %s", levelDataPath)
	}

	// 构建新的overrides内容
	overridesContent := "\n"

	// 将键排序
	keys := make([]string, 0, len(req.Overrides))
	for key := range req.Overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// 按排序后的键生成内容
	for _, key := range keys {
		value := req.Overrides[key]
		// 将值转换为字符串
		valueStr := fmt.Sprintf("%v", value)
		// 检查值的类型，数字、true、false不需要引号，其他字符串需要引号
		_, errFloat := strconv.ParseFloat(valueStr, 64)
		if valueStr != "true" && valueStr != "false" && errFloat != nil {
			valueStr = "\"" + valueStr + "\""
		}
		overridesContent += fmt.Sprintf("    %s=%s,\n", key, valueStr)
	}

	// 在overrides最后增加指定的配置项
	overridesContent += "    layout_mode=\"RestrictNodesByKey\",\n"
	overridesContent += "    roads=\"never\",\n"
	overridesContent += "    wormhole_prefab=\"tentacle_pillar\",\n"

	// 构建洞穴世界模板
	template := fmt.Sprintf(`return {
  background_node_range={ 0, 1 },
  desc="探查洞穴…… 一起！",
  hideminimap=false,
  id="DST_CAVE",
  location="cave",
  max_playlist_position=999,
  min_playlist_position=0,
  name="洞穴",
  numrandom_set_pieces=0,
  override_level_string=false,
  overrides={%s  },
  required_prefabs={ "multiplayer_portal" },
  settings_desc="探查洞穴…… 一起！",
  settings_id="DST_CAVE",
  settings_name="洞穴",
  substitutes={  },
  version=4,
  worldgen_desc="探查洞穴…… 一起！",
  worldgen_id="DST_CAVE",
  worldgen_name="洞穴"
}
`, overridesContent)

	// 写入新文件
	if err := ioutil.WriteFile(levelDataPath, []byte(template), 0644); err != nil {
		errorMsg := fmt.Sprintf("写入文件失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 500,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	log.Printf("成功创建或更新洞穴世界配置 - 存档: %s, 世界: %s, 配置项数量: %d", req.SaveName, req.WorldName, len(req.Overrides))

	// 返回创建或更新后的配置
	g.JSON(http.StatusOK, WorldOverridesResponse{
		Status: 200,
		Data:   req.Overrides,
		Msg:    "创建或更新洞穴世界配置成功",
	})
}

// GetServerIni 获取指定房间和世界的server.ini文件
func GetServerIni(g *gin.Context) {
	// 获取请求参数
	saveName := g.Query("savename")
	worldName := g.Query("worldname")

	// 记录请求参数
	log.Printf("接收到获取server.ini请求 - 存档名称: %s, 世界名称: %s", saveName, worldName)

	if saveName == "" || worldName == "" {
		errorMsg := "缺少必要的参数"
		if saveName == "" {
			errorMsg += ": savename"
		}
		if worldName == "" {
			if saveName == "" {
				errorMsg += " 和 worldname"
			} else {
				errorMsg += ": worldname"
			}
		}
		log.Printf("请求参数错误: %s", errorMsg)
		g.JSON(http.StatusOK, ServerIniResponse{
			Status: 400,
			Msg:    errorMsg,
		})
		return
	}

	// 构建leveldataoverride.lua文件路径
	serverIniPath := filepath.Join(dstSavePath, saveName, worldName, "server.ini")
	log.Printf("尝试读取文件: %s", serverIniPath)

	// 检查文件是否存在
	if _, err := os.Stat(serverIniPath); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("文件不存在: %s", serverIniPath)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, ServerIniResponse{
			Status: 404,
			Msg:    errorMsg,
		})
		return
	}

	// 读取并解析server.ini文件
	cfg, err := ini.Load(serverIniPath)
	if err != nil {
		errorMsg := fmt.Sprintf("读取server.ini失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, ServerIniResponse{
			Status: 500,
			Msg:    errorMsg,
		})
		return
	}

	// 将配置映射到结构体
	var config ServerIniConfig
	if err := cfg.MapTo(&config); err != nil {
		errorMsg := fmt.Sprintf("解析server.ini失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, ServerIniResponse{
			Status: 500,
			Msg:    errorMsg,
		})
		return
	}

	log.Printf("成功获取server.ini - 存档: %s, 世界: %s", saveName, worldName)

	// 返回配置
	g.JSON(http.StatusOK, ServerIniResponse{
		Status: 200,
		Data:   config,
	})
}

// CreateOrUpdateServerIni 创建或更新指定房间和世界的server.ini文件
func CreateOrUpdateServerIni(g *gin.Context) {
	// 解析请求体
	var req UpdateServerIniRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		log.Printf("解析请求体失败: %v", err)
		g.JSON(http.StatusOK, ServerIniResponse{
			Status: 400,
			Msg:    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 记录请求参数
	log.Printf("接收到创建或更新server.ini请求 - 存档名称: %s, 世界名称: %s",
		req.SaveName, req.WorldName)

	// 构建server.ini文件路径
	serverIniPath := filepath.Join(dstSavePath, req.SaveName, req.WorldName, "server.ini")
	log.Printf("尝试创建或更新文件: %s", serverIniPath)

	// 检查文件是否存在
	fileExists := true
	if _, err := os.Stat(serverIniPath); os.IsNotExist(err) {
		fileExists = false
	}

	// 检查目录是否存在，如果不存在则创建
	dirPath := filepath.Join(dstSavePath, req.SaveName, req.WorldName)
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			errorMsg := fmt.Sprintf("创建目录失败: %v", err)
			log.Print(errorMsg)
			g.JSON(http.StatusOK, ServerIniResponse{
				Status: 500,
				Msg:    errorMsg,
			})
			return
		}
		log.Printf("成功创建目录: %s", dirPath)
	}

	// 检查文件是否已存在
	if _, err := os.Stat(serverIniPath); err == nil {
		// 文件已存在，直接覆盖
		log.Printf("文件已存在，将直接覆盖: %s", serverIniPath)
	}

	// 创建新的ini文件
	cfg := ini.Empty()

	// 添加NETWORK部分
	networkSection, _ := cfg.NewSection("NETWORK")
	networkSection.NewKey("server_port", fmt.Sprintf("%d", req.Config.Network.ServerPort))

	// 添加SHARD部分
	shardSection, _ := cfg.NewSection("SHARD")
	shardSection.NewKey("is_master", fmt.Sprintf("%t", req.Config.Shard.IsMaster))
	shardSection.NewKey("name", req.Config.Shard.Name)
	shardSection.NewKey("id", fmt.Sprintf("%d", req.Config.Shard.ID))

	// 添加ACCOUNT部分
	accountSection, _ := cfg.NewSection("ACCOUNT")
	accountSection.NewKey("encode_user_path", fmt.Sprintf("%t", req.Config.Account.EncodeUserPath))

	// 添加STEAM部分
	steamSection, _ := cfg.NewSection("STEAM")
	steamSection.NewKey("master_server_port", fmt.Sprintf("%d", req.Config.Steam.MasterServerPort))
	steamSection.NewKey("authentication_port", fmt.Sprintf("%d", req.Config.Steam.AuthenticationPort))

	// 写入文件
	if err := cfg.SaveTo(serverIniPath); err != nil {
		errorMsg := fmt.Sprintf("写入server.ini失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, ServerIniResponse{
			Status: 500,
			Msg:    errorMsg,
		})
		return
	}

	// 根据文件是否存在返回不同的消息
	var successMsg string
	if fileExists {
		log.Printf("成功更新server.ini - 存档: %s, 世界: %s", req.SaveName, req.WorldName)
		successMsg = "更新server.ini成功"
	} else {
		log.Printf("成功创建server.ini - 存档: %s, 世界: %s", req.SaveName, req.WorldName)
		successMsg = "创建server.ini成功"
	}

	// 返回创建或更新后的配置
	g.JSON(http.StatusOK, ServerIniResponse{
		Status: 200,
		Data:   req.Config,
		Msg:    successMsg,
	})
}

// 保留这些函数以保持向后兼容性，但它们现在都调用CreateOrUpdateServerIni

// UpdateServerIni 更新指定房间和世界的server.ini文件 (现在调用CreateOrUpdateServerIni)
func UpdateServerIni(g *gin.Context) {
	CreateOrUpdateServerIni(g)
}

// CreateServerIni 创建指定房间和世界的server.ini文件 (现在调用CreateOrUpdateServerIni)
func CreateServerIni(g *gin.Context) {
	CreateOrUpdateServerIni(g)
}

// copyFile 复制文件的辅助函数
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// CreateTemplateForestWorld 从模板创建森林世界的leveldataoverride.lua文件
func CreateTemplateForestWorld(g *gin.Context) {
	// 解析请求体
	var req TemplateWorldRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		log.Printf("解析请求体失败: %v", err)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 400,
			Data:   nil,
			Msg:    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 记录请求参数
	log.Printf("接收到从模板创建森林世界请求 - 存档名称: %s, 世界名称: %s",
		req.SaveName, req.WorldName)

	// 构建target目录路径
	targetDir := filepath.Join(dstSavePath, req.SaveName, req.WorldName)
	log.Printf("目标目录: %s", targetDir)

	// 检查目录是否存在，如果不存在则创建
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			errorMsg := fmt.Sprintf("创建目录失败: %v", err)
			log.Print(errorMsg)
			g.JSON(http.StatusOK, WorldOverridesResponse{
				Status: 500,
				Data:   nil,
				Msg:    errorMsg,
			})
			return
		}
		log.Printf("成功创建目录: %s", targetDir)
	}

	// 构建源文件路径
	sourceFile := "./template/forest/leveldataoverride.lua"
	log.Printf("源文件: %s", sourceFile)

	// 检查源文件是否存在
	if _, err := os.Stat(sourceFile); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("模板文件不存在: %s", sourceFile)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 404,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	// 构建目标文件路径
	targetFile := filepath.Join(targetDir, "leveldataoverride.lua")
	log.Printf("目标文件: %s", targetFile)

	// 复制文件
	if err := copyFile(sourceFile, targetFile); err != nil {
		errorMsg := fmt.Sprintf("复制文件失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 500,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	log.Printf("成功从模板创建森林世界配置 - 存档: %s, 世界: %s", req.SaveName, req.WorldName)

	// 返回成功消息
	g.JSON(http.StatusOK, WorldOverridesResponse{
		Status: 200,
		Data:   nil,
		Msg:    "从模板创建森林世界配置成功",
	})
}

// CreateTemplateCaveWorld 从模板创建洞穴世界的leveldataoverride.lua文件
func CreateTemplateCaveWorld(g *gin.Context) {
	// 解析请求体
	var req TemplateWorldRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		log.Printf("解析请求体失败: %v", err)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 400,
			Data:   nil,
			Msg:    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 记录请求参数
	log.Printf("接收到从模板创建洞穴世界请求 - 存档名称: %s, 世界名称: %s",
		req.SaveName, req.WorldName)

	// 构建target目录路径
	targetDir := filepath.Join(dstSavePath, req.SaveName, req.WorldName)
	log.Printf("目标目录: %s", targetDir)

	// 检查目录是否存在，如果不存在则创建
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			errorMsg := fmt.Sprintf("创建目录失败: %v", err)
			log.Print(errorMsg)
			g.JSON(http.StatusOK, WorldOverridesResponse{
				Status: 500,
				Data:   nil,
				Msg:    errorMsg,
			})
			return
		}
		log.Printf("成功创建目录: %s", targetDir)
	}

	// 构建源文件路径
	sourceFile := "./template/cave/leveldataoverride.lua"
	log.Printf("源文件: %s", sourceFile)

	// 检查源文件是否存在
	if _, err := os.Stat(sourceFile); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("模板文件不存在: %s", sourceFile)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 404,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	// 构建目标文件路径
	targetFile := filepath.Join(targetDir, "leveldataoverride.lua")
	log.Printf("目标文件: %s", targetFile)

	// 复制文件
	if err := copyFile(sourceFile, targetFile); err != nil {
		errorMsg := fmt.Sprintf("复制文件失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, WorldOverridesResponse{
			Status: 500,
			Data:   nil,
			Msg:    errorMsg,
		})
		return
	}

	log.Printf("成功从模板创建洞穴世界配置 - 存档: %s, 世界: %s", req.SaveName, req.WorldName)

	// 返回成功消息
	g.JSON(http.StatusOK, WorldOverridesResponse{
		Status: 200,
		Data:   nil,
		Msg:    "从模板创建洞穴世界配置成功",
	})
}

// DeleteWorldRequest 删除世界请求结构
type DeleteWorldRequest struct {
	SaveName  string `json:"savename" binding:"required"`
	WorldName string `json:"worldname" binding:"required"`
}

// DeleteWorldResponse 删除世界响应结构
type DeleteWorldResponse struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
}

// DeleteWorld 删除指定存档和世界
func DeleteWorld(g *gin.Context) {
	// 解析请求体
	var req DeleteWorldRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		log.Printf("解析请求体失败: %v", err)
		g.JSON(http.StatusOK, DeleteWorldResponse{
			Status: 400,
			Msg:    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 记录请求参数
	log.Printf("接收到删除世界请求 - 存档名称: %s, 世界名称: %s",
		req.SaveName, req.WorldName)

	// 构建世界目录路径
	worldPath := filepath.Join(dstSavePath, req.SaveName, req.WorldName)
	log.Printf("尝试删除目录: %s", worldPath)

	// 检查目录是否存在
	if _, err := os.Stat(worldPath); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("世界目录不存在: %s", worldPath)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, DeleteWorldResponse{
			Status: 404,
			Msg:    errorMsg,
		})
		return
	}

	// 直接删除世界目录
	log.Printf("直接删除世界目录: %s", worldPath)

	// 使用os.RemoveAll删除目录及其内容
	if err := os.RemoveAll(worldPath); err != nil {
		errorMsg := fmt.Sprintf("删除世界目录失败: %v", err)
		log.Print(errorMsg)
		g.JSON(http.StatusOK, DeleteWorldResponse{
			Status: 500,
			Msg:    errorMsg,
		})
		return
	}

	log.Printf("成功删除世界 - 存档: %s, 世界: %s", req.SaveName, req.WorldName)

	// 返回成功响应
	g.JSON(http.StatusOK, DeleteWorldResponse{
		Status: 200,
		Msg:    fmt.Sprintf("成功删除世界: %s/%s", req.SaveName, req.WorldName),
	})
}
