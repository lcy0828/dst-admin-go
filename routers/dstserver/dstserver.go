package dstserver

import (
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	configFile := "./conf/app.conf"
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
	Name string `json:"name"` // 世界名称
	Type string `json:"type"` // 世界类型 (forest/cave)
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
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}

	// 构建黑名单列表文件路径
	blockListPath := filepath.Join(dstSavePath, saveName, "blocklist.txt")

	// 读取请求体
	var req struct {
		Blocked []string `json:"blocked" binding:"required"`
	}
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}

	// 将黑名单列表写入文件
	content := strings.Join(req.Blocked, "\n")
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
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}

	// 构建白名单列表文件路径
	whiteListPath := filepath.Join(dstSavePath, saveName, "whitelist.txt")

	// 读取请求体
	var req struct {
		Whitelisted []string `json:"whitelisted" binding:"required"`
	}
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}

	// 将白名单列表写入文件
	content := strings.Join(req.Whitelisted, "\n")
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
	saveName := g.Query("savename")
	if saveName == "" {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "存档名称不能为空",
		})
		return
	}

	// 构建cluster_token.txt路径
	tokenPath := filepath.Join(dstSavePath, saveName, "cluster_token.txt")

	// 读取请求体
	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if err := g.BindJSON(&req); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"status": 400,
			"msg":    "请求参数错误",
		})
		return
	}

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
