package mod

import (
	"bytes"
	"dont/models"
	"dont/pkg/configpath"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"github.com/gocolly/colly"
	"github.com/hpcloud/tail"
	"gorm.io/gorm"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// 日志前缀常量
const (
	LogPrefix       = "[MOD]"
	LogSearchPrefix = "[MOD][SEARCH]"
	LogDownPrefix   = "[MOD][DOWNLOAD]"
	LogCachePrefix  = "[MOD][CACHE]"
	LogTmuxPrefix   = "[MOD][TMUX]"
)

// 模组相关配置变量
var (
	appID           string // 饥荒联机版的AppID
	steamCmdPath    string // Steam命令行工具路径
	luaShPath       string // Lua脚本路径
	workshopContent string // Workshop内容路径
	workshopModPath string // Workshop模组安装路径
	tmuxSessionName string // Tmux会话名称
)

var wg sync.WaitGroup
var mutex sync.Mutex

var (
	fileName string
	p1       int
)

func init() {
	// 默认值
	appID = "322330"
	steamCmdPath = "/opt/go-dont/steam"
	luaShPath = "/opt/go-dont/lua-sh"
	workshopContent = "/root/Steam/steamapps/workshop/content/322330"
	workshopModPath = "/root/Steam/"
	tmuxSessionName = "DST_MODDOWN"
	fileName = "/root/Steam/logs/workshop_log.txt"

	// 从配置文件读取
	configFile := configpath.Current()
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			// 读取模组相关配置
			modSection := cfg.Section("mod")

			if modSection.HasKey("APP_ID") {
				appID = modSection.Key("APP_ID").String()
				log.Printf("%s 从配置文件加载饥荒AppID: %s", LogPrefix, appID)
			}

			if modSection.HasKey("STEAM_CMD_PATH") {
				steamCmdPath = modSection.Key("STEAM_CMD_PATH").String()
				log.Printf("%s 从配置文件加载Steam命令行路径: %s", LogPrefix, steamCmdPath)
			}

			if modSection.HasKey("LUA_SH_PATH") {
				luaShPath = modSection.Key("LUA_SH_PATH").String()
				log.Printf("%s 从配置文件加载Lua脚本路径: %s", LogPrefix, luaShPath)
			}

			if modSection.HasKey("WORKSHOP_CONTENT") {
				workshopContent = modSection.Key("WORKSHOP_CONTENT").String()
				log.Printf("%s 从配置文件加载Workshop内容路径: %s", LogPrefix, workshopContent)
			}

			if modSection.HasKey("WORKSHOP_MOD_PATH") {
				workshopModPath = modSection.Key("WORKSHOP_MOD_PATH").String()
				log.Printf("%s 从配置文件加载Workshop模组安装路径: %s", LogPrefix, workshopModPath)
			}

			if modSection.HasKey("TMUX_SESSION_NAME") {
				tmuxSessionName = modSection.Key("TMUX_SESSION_NAME").String()
				log.Printf("%s 从配置文件加载Tmux会话名称: %s", LogPrefix, tmuxSessionName)
			}

			if modSection.HasKey("WORKSHOP_LOG_FILE") {
				// 处理路径中的转义字符
				fileName = strings.ReplaceAll(modSection.Key("WORKSHOP_LOG_FILE").String(), "\\", "")
				log.Printf("%s 从配置文件加载模组下载日志文件: %s", LogPrefix, fileName)
			}
		} else {
			log.Printf("%s 警告: 无法读取配置文件: %v, 使用默认配置", LogPrefix, err)
		}
	} else {
		log.Printf("%s 警告: 配置文件不存在, 使用默认配置", LogPrefix)
	}

	// 命令行参数优先级更高
	flag.StringVar(&fileName, "f", fileName, "日志文件")
}

// Vote 模组评分结构
type Vote struct {
	Num  string `form:"num" json:"num"`
	Star int    `form:"star" json:"star"`
}

// Searchmodinfo 模组搜索结果信息
type Searchmodinfo struct {
	Auth      string `form:"auth" json:"auth"`             // 作者
	Id        string `form:"id" json:"id"`                 // 模组id
	Img       string `form:"img" json:"img"`               // 模组图片
	Name      string `form:"name" json:"name"`             // 模组名字
	Sub       string `form:"sub" json:"sub"`               // 订阅数
	Time      string `form:"time" json:"time"`             // 更新时间
	Version   string `form:"version" json:"version"`       // 版本
	Describe  string `form:"describe" json:"describe"`     // 描述
	RatingImg string `form:"rating_img" json:"rating_img"` // 评分图片

	Vote `form:"vote" json:"vote"`
}

// Modsearch 模组搜索请求参数
type Modsearch struct {
	Modname string `form:"modname" json:"modname" uri:"modname" xml:"modname" binding:"required"`
}

// Moddown 模组下载请求参数
type Moddown struct {
	Modid   string `form:"modid" json:"modid" uri:"modid" xml:"modid" binding:"required"`
	Refresh string `form:"refresh" json:"refresh" uri:"refresh" xml:"refresh" binding:"required"`
	Version string `form:"version" json:"version" uri:"version" xml:"version" binding:"required"`
}

// APIResponse 标准API响应结构
type APIResponse struct {
	Status int    `json:"status"`
	Info   string `json:"modinfo"`
}

// ModAddRequest 添加模组到服务器的请求参数
type ModAddRequest struct {
	Auth    string `json:"auth"`    // 作者
	Id      string `json:"id"`      // 模组id
	Img     string `json:"img"`     // 模组图片
	Name    string `json:"name"`    // 模组名字
	Sub     string `json:"sub"`     // 订阅数
	Time    string `json:"time"`    // 更新时间
	Version string `json:"version"` // 版本
	Rating  string `json:"rating"`  // 评分
}

// ModCustomConfigRequest 模组自定义配置的请求参数
type ModCustomConfigRequest struct {
	Modid                string                 `json:"modid"`                 // 模组ID
	ConfigurationOptions map[string]interface{} `json:"configuration_options"` // 配置选项
	Enabled              bool                   `json:"enabled"`               // 是否启用
}

// ModToggleRequest 模组启用/禁用请求参数
type ModToggleRequest struct {
	Modid   string `json:"modid"`   // 模组ID
	Enabled bool   `json:"enabled"` // 是否启用
}

// BytesToString 字节数组转字符串
func BytesToString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// ModInfoJSON 模组信息JSON结构
type ModInfoJSON struct {
	Name        string   `json:"name"`
	Author      string   `json:"author"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// ParseModInfo 解析模组信息JSON并更新数据库
func ParseModInfo(modid string, jsonData string, subscribers string) bool {
	// 解析JSON数据
	var modInfo map[string]interface{}
	if err := json.Unmarshal([]byte(jsonData), &modInfo); err != nil {
		log.Printf("%s 解析模组信息JSON失败 - 模组ID: %s, 错误: %v", LogPrefix, modid, err)
		return false
	}

	// 提取基本信息
	name := ""
	author := ""
	version := ""
	description := ""
	var tags []string

	if val, ok := modInfo["name"].(string); ok {
		name = val
	}
	if val, ok := modInfo["author"].(string); ok {
		author = val
	}
	if val, ok := modInfo["version"].(string); ok {
		version = val
	}
	if val, ok := modInfo["description"].(string); ok {
		description = val
	}
	if tagsArray, ok := modInfo["server_filter_tags"].([]interface{}); ok {
		for _, tag := range tagsArray {
			if tagStr, ok := tag.(string); ok {
				tags = append(tags, tagStr)
			}
		}
	}

	// 将标签数组转换为逗号分隔的字符串
	tagsStr := strings.Join(tags, ",")

	// 将订阅者数量转换为整数
	// 先移除逗号和空格
	subsClean := strings.ReplaceAll(subscribers, ",", "")
	subsClean = strings.TrimSpace(subsClean)
	subsCount, err := strconv.Atoi(subsClean)
	if err != nil {
		log.Printf("%s 转换订阅者数量失败 - 模组ID: %s, 原始值: %s, 错误: %v",
			LogPrefix, modid, subscribers, err)
		subsCount = 0 // 转换失败时使用默认值
	}

	// 更新数据库
	success := models.UpdateModInfo(
		modid,
		name,
		author,
		version,
		description,
		tagsStr,
		subsCount,
		jsonData, // 存储完整的JSON配置信息
	)

	// 提取配置选项的默认值
	defaultConfig := make(map[string]interface{})

	if configOptions, ok := modInfo["configuration_options"].([]interface{}); ok {
		for _, option := range configOptions {
			if optionMap, ok := option.(map[string]interface{}); ok {
				// 获取选项名称和默认值
				if name, ok := optionMap["name"].(string); ok {
					if defaultVal, exists := optionMap["default"]; exists {
						defaultConfig[name] = defaultVal
					}
				}
			}
		}
	}

	// 将默认配置转换为JSON字符串
	configJSON, err := json.Marshal(defaultConfig)
	if err != nil {
		log.Printf("%s 将默认配置转换为JSON失败 - 模组ID: %s, 错误: %v",
			LogPrefix, modid, err)
	} else {
		// 保存模组自定义配置
		configSuccess, err := models.SaveModCustomConfig(modid, string(configJSON), false)
		if err != nil {
			log.Printf("%s 保存模组自定义配置失败 - 模组ID: %s, 错误: %v",
				LogPrefix, modid, err)
		} else if configSuccess {
			log.Printf("%s 保存模组自定义配置成功 - 模组ID: %s, 选项数: %d",
				LogPrefix, modid, len(defaultConfig))
		}
	}

	if success {
		log.Printf("%s 更新模组完整信息成功 - 模组ID: %s, 名称: %s",
			LogPrefix, modid, name)
	} else {
		log.Printf("%s 更新模组完整信息失败 - 模组ID: %s", LogPrefix, modid)
	}

	return success
}

// AddModToServer 添加模组到服务器处理函数
func AddModToServer(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到添加模组到服务器请求 来自IP: %s", LogPrefix, clientIP)

	// 解析请求体
	var request ModAddRequest
	if err := g.ShouldBindJSON(&request); err != nil {
		log.Printf("%s 解析请求体失败: %v", LogPrefix, err)
		g.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	// 检查是否提供了模组ID
	if request.Id == "" {
		log.Printf("%s 未提供模组ID IP: %s", LogPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"error": "请提供模组ID"})
		return
	}

	// 先将模组信息保存到dont_mod_info表中
	// 将订阅者数量转换为整数
	subsClean := strings.ReplaceAll(request.Sub, ",", "")
	subsClean = strings.TrimSpace(subsClean)
	subsCount, err := strconv.Atoi(subsClean)
	if err != nil {
		log.Printf("%s 转换订阅者数量失败 - 模组ID: %s, 原始值: %s, 错误: %v",
			LogPrefix, request.Id, request.Sub, err)
		subsCount = 0 // 转换失败时使用默认值
	}

	// 更新dont_mod_info表
	modInfoSuccess := models.UpdateModInfo(
		request.Id,
		request.Name,
		request.Auth,
		request.Version,
		"", // 描述信息可能不完整，可以留空
		"", // 标签信息可能不完整，可以留空
		subsCount,
		"", // 配置信息为空，稍后会更新
	)

	if !modInfoSuccess {
		log.Printf("%s 更新模组信息到dont_mod_info表失败 - 模组ID: %s", LogPrefix, request.Id)
	} else {
		log.Printf("%s 更新模组信息到dont_mod_info表成功 - 模组ID: %s", LogPrefix, request.Id)
	}
	// 获取模组配置信息
	var configToSave string
	// 从数据库中获取配置信息
	configStr, err := models.GetModConfig(request.Id)
	if err == nil && configStr != "" {
		configToSave = configStr
	} else {
		// 如果没有配置信息，使用空对象
		configToSave = "{}"
	}
	log.Printf("%s 开始下载模组 - 模组ID: %s", LogPrefix, request.Id)
	configToSave = DownloadMod2(request.Id, "true", request.Version)

	// 如果模组下载成功，将完整的模组信息写入到dont_mod_info表中
	if p1 == 1 && configToSave != "" {
		log.Printf("%s 模组下载成功，将完整信息写入到dont_mod_info表 - 模组ID: %s", LogPrefix, request.Id)
		// 使用ParseModInfo函数解析模组信息并更新数据库
		ParseModInfo(request.Id, configToSave, request.Sub)
	}

	// 添加模组到服务器
	success := models.AddServerMod(
		request.Id,
		request.Name,
		request.Auth,
		request.Version,
		request.Img,
		request.Sub,
		request.Time,
		request.Rating,
		configToSave, // 传递模组配置信息
	)

	// 构建响应
	statusCode := http.StatusOK
	response := gin.H{
		"success":    success,
		"downloaded": p1 == 1, // 模组是否已下载
	}

	if !success {
		response["error"] = "添加模组到服务器失败"
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 添加模组到服务器请求处理完成 - 模组ID: %s, 成功: %v, 耗时: %v",
		LogPrefix, request.Id, success, elapsedTime)

	g.JSON(statusCode, response)
}

// GetServerMods 获取服务器模组列表处理函数
func GetServerMods(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到获取服务器模组列表请求 来自IP: %s", LogPrefix, clientIP)

	// 获取服务器模组列表
	mods, err := models.GetServerMods()
	if err != nil {
		log.Printf("%s 获取服务器模组列表失败: %v", LogPrefix, err)
		g.JSON(http.StatusInternalServerError, gin.H{"error": "获取服务器模组列表失败"})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 获取服务器模组列表请求处理完成 - 找到: %d 个模组, 耗时: %v",
		LogPrefix, len(mods), elapsedTime)

	g.JSON(http.StatusOK, mods)
}

// GetModConfig 获取模组配置信息处理函数
func GetModConfig(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到获取模组配置信息请求 来自IP: %s", LogPrefix, clientIP)

	// 获取模组ID参数
	modid := g.Query("modid")
	if modid == "" {
		log.Printf("%s 未提供模组ID IP: %s", LogPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"status": 400, "modinfo": "请提供模组ID"})
		return
	}

	// 获取模组配置信息
	configJSON, err := models.GetModConfig(modid)
	if err != nil {
		log.Printf("%s 获取模组配置信息失败: %v", LogPrefix, err)
		g.JSON(http.StatusInternalServerError, gin.H{"status": 500, "modinfo": "获取模组配置信息失败"})
		return
	}

	// 如果配置信息为空，返回空对象
	if configJSON == "" {
		configJSON = "{}"
	}

	// 尝试将配置信息解析为JSON对象，确保是有效的JSON
	var configObj interface{}
	if err := json.Unmarshal([]byte(configJSON), &configObj); err != nil {
		log.Printf("%s 解析模组配置信息JSON失败: %v", LogPrefix, err)
		configObj = map[string]interface{}{}
	}

	// 尝试格式化JSON，使其更易读
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, []byte(configJSON), "", "  "); err == nil {
		configJSON = prettyJSON.String()
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 获取模组配置信息请求处理完成 - 模组ID: %s, 耗时: %v",
		LogPrefix, modid, elapsedTime)

	// 如果客户端请求原始JSON，则返回原始JSON字符串
	if g.Query("raw") == "true" {
		g.Header("Content-Type", "application/json")
		g.String(http.StatusOK, configJSON)
		return
	}

	// 否则返回解析后的JSON对象，避免转义字符
	g.JSON(http.StatusOK, gin.H{
		"status":  200,
		"modinfo": configObj,
	})
}

// SaveModCustomConfig 保存模组自定义配置处理函数
func SaveModCustomConfig(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到保存模组自定义配置请求 来自IP: %s", LogPrefix, clientIP)

	// 解析请求体
	var request ModCustomConfigRequest
	if err := g.ShouldBindJSON(&request); err != nil {
		log.Printf("%s 解析请求体失败: %v", LogPrefix, err)
		g.JSON(http.StatusBadRequest, gin.H{"status": 400, "message": "请求格式错误"})
		return
	}

	// 检查是否提供了模组ID
	if request.Modid == "" {
		log.Printf("%s 未提供模组ID IP: %s", LogPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"status": 400, "message": "请提供模组ID"})
		return
	}

	// 将配置选项转换为JSON字符串
	configJSON, err := json.Marshal(request.ConfigurationOptions)
	if err != nil {
		log.Printf("%s 将配置选项转换为JSON失败: %v", LogPrefix, err)
		g.JSON(http.StatusInternalServerError, gin.H{"status": 500, "message": "处理配置选项失败"})
		return
	}

	// 保存模组自定义配置
	success, err := models.SaveModCustomConfig(request.Modid, string(configJSON), request.Enabled)
	if err != nil {
		log.Printf("%s 保存模组自定义配置失败: %v", LogPrefix, err)
		g.JSON(http.StatusInternalServerError, gin.H{"status": 500, "message": "保存模组自定义配置失败"})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 保存模组自定义配置请求处理完成 - 模组ID: %s, 成功: %v, 耗时: %v",
		LogPrefix, request.Modid, success, elapsedTime)

	// 返回保存结果
	g.JSON(http.StatusOK, gin.H{
		"status":  200,
		"modinfo": request.ConfigurationOptions,
	})
}

// GetModCustomConfig 获取模组自定义配置处理函数
func GetModCustomConfig(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到获取模组自定义配置请求 来自IP: %s", LogPrefix, clientIP)

	// 获取模组ID参数
	modid := g.Query("modid")
	if modid == "" {
		log.Printf("%s 未提供模组ID IP: %s", LogPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"status": 400, "modinfo": "请提供模组ID"})
		return
	}

	// 获取模组自定义配置
	modConfig, err := models.GetModCustomConfig(modid)
	if err != nil {
		// 如果没有找到配置，返回空对象
		if err == gorm.ErrRecordNotFound {
			log.Printf("%s 模组自定义配置不存在 - 模组ID: %s", LogPrefix, modid)
			g.JSON(http.StatusOK, gin.H{
				"status": 200,
				"modinfo": map[string]interface{}{
					"configuration_options": map[string]interface{}{},
					"enabled":               false, // 默认为禁用状态
				},
			})
			return
		}

		log.Printf("%s 获取模组自定义配置失败: %v", LogPrefix, err)
		g.JSON(http.StatusInternalServerError, gin.H{"status": 500, "modinfo": "获取模组自定义配置失败"})
		return
	}

	// 解析配置选项JSON
	var configOptions map[string]interface{}
	if modConfig.ConfigurationOptions == "" {
		configOptions = map[string]interface{}{}
	} else {
		if err := json.Unmarshal([]byte(modConfig.ConfigurationOptions), &configOptions); err != nil {
			log.Printf("%s 解析模组自定义配置失败: %v", LogPrefix, err)
			configOptions = map[string]interface{}{}
		}
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 获取模组自定义配置请求处理完成 - 模组ID: %s, 耗时: %v",
		LogPrefix, modid, elapsedTime)

	// 返回模组自定义配置
	g.JSON(http.StatusOK, gin.H{
		"status": 200,
		"modinfo": gin.H{
			"configuration_options": configOptions,
			"enabled":               modConfig.Enabled,
		},
	})
}

// GenerateModConfigFile 生成模组配置文件处理函数
func GenerateModConfigFile(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到生成模组配置文件请求 来自IP: %s", LogPrefix, clientIP)

	// 获取所有模组的自定义配置
	configs, err := models.GetAllModConfigs()
	if err != nil {
		log.Printf("%s 获取所有模组自定义配置失败: %v", LogPrefix, err)
		g.JSON(http.StatusInternalServerError, gin.H{"status": 500, "message": "获取模组配置失败"})
		return
	}
	checkconfigs, err := models.GetServerMods()
	if err != nil {
		log.Printf("%s 获取已下载模组状态失败: %v", LogPrefix, err)
		g.JSON(http.StatusInternalServerError, gin.H{"status": 500, "message": "获取模组状态失败"})
		return
	}
	// 构建配置文件内容
	configFileContent := "return {\n"

	// 遍历所有模组配置
	for i, config := range configs {
		// 解析配置选项JSON
		checkisenable := false
		var configOptions map[string]interface{}
		if config.ConfigurationOptions == "" {
			configOptions = map[string]interface{}{}
		} else {
			if err := json.Unmarshal([]byte(config.ConfigurationOptions), &configOptions); err != nil {
				log.Printf("%s 解析模组自定义配置失败 - 模组ID: %s, 错误: %v", LogPrefix, config.Modid, err)
				configOptions = map[string]interface{}{}
			}
		}
		for _, checkconfig := range checkconfigs {
			if checkconfig.Modid == config.Modid && checkconfig.Enabled == true {
				checkisenable = true
			}
		}
		if !checkisenable {
			continue
		}

		// 添加模组配置
		configFileContent += fmt.Sprintf("  [\"workshop-%s\"]={\n", config.Modid)
		configFileContent += "    configuration_options={\n"

		// 添加配置选项
		optionCount := 0
		for key, value := range configOptions {
			optionCount++

			// 根据值的类型进行不同的处理
			switch v := value.(type) {
			case string:
				configFileContent += fmt.Sprintf("      %s=\"%s\"%s\n", key, v, getComma(optionCount < len(configOptions)))
			case bool:
				configFileContent += fmt.Sprintf("      %s=%t%s\n", key, v, getComma(optionCount < len(configOptions)))
			case float64:
				// 如果是整数，则不显示小数点
				if v == float64(int(v)) {
					configFileContent += fmt.Sprintf("      %s=%d%s\n", key, int(v), getComma(optionCount < len(configOptions)))
				} else {
					configFileContent += fmt.Sprintf("      %s=%.1f%s\n", key, v, getComma(optionCount < len(configOptions)))
				}
			default:
				configFileContent += fmt.Sprintf("      %s=%v%s\n", key, v, getComma(optionCount < len(configOptions)))
			}
		}

		configFileContent += "    },\n"
		configFileContent += fmt.Sprintf("    enabled=%t \n", config.Enabled)
		configFileContent += "  }" + getComma(i < len(configs)-1) + "\n"
	}

	configFileContent += "}"

	elapsedTime := time.Since(startTime)
	log.Printf("%s 生成模组配置文件请求处理完成 - 找到: %d 个模组配置, 耗时: %v",
		LogPrefix, len(configs), elapsedTime)

	// 返回配置文件内容
	g.JSON(http.StatusOK, gin.H{
		"status":  200,
		"modinfo": configFileContent,
	})
}

// getComma 根据条件返回逗号
func getComma(needComma bool) string {
	if needComma {
		return ","
	}
	return ""
}

// DeleteServerMod 删除服务器模组处理函数
func DeleteServerMod(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到删除服务器模组请求 来自IP: %s", LogPrefix, clientIP)

	// 获取模组ID参数
	modid := g.Query("modid")

	// 如果没有从查询参数获取到，则尝试从请求体中获取
	if modid == "" {
		var request struct {
			Modid string `json:"modid"`
		}
		if err := g.ShouldBindJSON(&request); err == nil && request.Modid != "" {
			modid = request.Modid
			log.Printf("%s 从请求体获取到模组ID: %s", LogPrefix, modid)
		} else if err != nil {
			log.Printf("%s 解析请求体失败: %v", LogPrefix, err)
		}
	} else {
		log.Printf("%s 从查询参数获取到模组ID: %s", LogPrefix, modid)
	}

	// 检查是否提供了模组ID
	if modid == "" {
		log.Printf("%s 未提供模组ID IP: %s", LogPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"status": 400, "message": "请提供模组ID"})
		return
	}

	// 从数据库中删除模组
	success, err := models.DeleteServerMod(modid)
	if err != nil {
		log.Printf("%s 删除模组失败: %v", LogPrefix, err)
		g.JSON(http.StatusInternalServerError, gin.H{"status": 500, "message": "删除模组失败"})
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 删除模组请求处理完成 - 模组ID: %s, 成功: %v, 耗时: %v",
		LogPrefix, modid, success, elapsedTime)

	// 返回删除结果
	if success {
		g.JSON(http.StatusOK, gin.H{"status": 200, "message": "删除模组成功"})
	} else {
		g.JSON(http.StatusNotFound, gin.H{"status": 404, "message": "模组不存在"})
	}
}

// ToggleModEnabled 启用/禁用模组处理函数
func ToggleModEnabled(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到启用/禁用模组请求 来自IP: %s", LogPrefix, clientIP)

	// 解析请求体
	var request ModToggleRequest
	if err := g.ShouldBindJSON(&request); err != nil {
		log.Printf("%s 解析请求体失败: %v", LogPrefix, err)
		g.JSON(http.StatusBadRequest, gin.H{"status": 400, "message": "请求格式错误"})
		return
	}

	// 检查是否提供了模组ID
	if request.Modid == "" {
		log.Printf("%s 未提供模组ID IP: %s", LogPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"status": 400, "message": "请提供模组ID"})
		return
	}

	// 更新模组启用状态
	success, err := models.UpdateModEnabled(request.Modid, request.Enabled)
	if err != nil {
		log.Printf("%s 更新模组启用状态失败: %v", LogPrefix, err)

		// 如果是模组不存在的错误，返回404
		if strings.Contains(err.Error(), "模组不存在") {
			g.JSON(http.StatusNotFound, gin.H{"status": 404, "message": "模组不存在"})
		} else {
			g.JSON(http.StatusInternalServerError, gin.H{"status": 500, "message": "更新模组启用状态失败"})
		}
		return
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 更新模组启用状态请求处理完成 - 模组ID: %s, 启用状态: %v, 成功: %v, 耗时: %v",
		LogPrefix, request.Modid, request.Enabled, success, elapsedTime)

	// 返回更新结果
	var statusMessage string
	if request.Enabled {
		statusMessage = "模组已启用"
	} else {
		statusMessage = "模组已禁用"
	}

	g.JSON(http.StatusOK, gin.H{
		"status":  200,
		"message": statusMessage,
		"modid":   request.Modid,
		"enabled": request.Enabled,
	})
}

// SearchMod 搜索模组处理函数
func SearchMod(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到模组搜索请求 来自IP: %s", LogSearchPrefix, clientIP)

	keyword := g.Query("keyword")
	page := g.DefaultQuery("page", "1")

	// 如果没有提供关键字，则尝试从表单或JSON中获取
	if keyword == "" {
		var form Modsearch
		if err := g.Bind(&form); err == nil && form.Modname != "" {
			keyword = form.Modname
			log.Printf("%s 从请求体获取到关键字: %s", LogSearchPrefix, keyword)
		} else if err != nil {
			log.Printf("%s 请求体解析失败: %v", LogSearchPrefix, err)
		}
	} else {
		log.Printf("%s 从查询参数获取到关键字: %s, 页码: %s", LogSearchPrefix, keyword, page)
	}

	// 检查是否有关键字
	if keyword == "" {
		log.Printf("%s 未提供搜索关键字 IP: %s", LogSearchPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"error": "请提供搜索关键字"})
		return
	}

	// 使用map存储模组信息，以模组ID为键
	modInfoMap := make(map[string]*Searchmodinfo)
	var modList []string // 保持模组顺序

	// 互斥锁用于保护map的并发访问
	var mapMutex sync.Mutex

	// 初始化爬虫
	c := colly.NewCollector(
		colly.AllowedDomains("steamcommunity.com"),
	)
	c.Async = true
	c.Limit(&colly.LimitRule{
		Parallelism: 2, // 增加并行度提高爬取速度
		RandomDelay: 1 * time.Second,
	})

	c.OnRequest(func(r *colly.Request) {
		r.Headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		log.Printf("%s 正在访问: %s", LogSearchPrefix, r.URL.String())
	})

	c.OnError(func(r *colly.Response, err error) {
		log.Printf("%s 爬取错误: %v URL: %s 状态码: %d",
			LogSearchPrefix, err, r.Request.URL, r.StatusCode)
	})

	// 解析模组基础信息
	c.OnHTML("div[class=workshopItem]", func(e *colly.HTMLElement) {
		modID := e.ChildAttr("a", "data-publishedfileid")
		if modID == "" {
			return
		}

		mapMutex.Lock()

		// 检查是否已存在，如果不存在则创建新项
		if _, exists := modInfoMap[modID]; !exists {
			modInfoMap[modID] = &Searchmodinfo{Id: modID}
			modList = append(modList, modID) // 保持顺序
		}

		// 更新模组基本信息
		modInfoMap[modID].Img = e.ChildAttr("a>div>img", "src")
		modInfoMap[modID].Name = e.ChildText("a[class=item_link]>div")
		modInfoMap[modID].Auth = e.ChildText("div>a[class=workshop_author_link]")

		// 提取评分图片
		ratingImg := e.ChildAttr("img.fileRating", "src")
		if ratingImg == "" {
			// 尝试另一种选择器格式
			ratingImg = e.ChildAttr("img[class=fileRating]", "src")
		}
		// 如果仍然找不到，尝试更宽泛的搜索
		if ratingImg == "" {
			e.ForEach("img", func(_ int, img *colly.HTMLElement) {
				if img.Attr("class") == "fileRating" {
					ratingImg = img.Attr("src")
				}
			})
		}
		if ratingImg != "" {
			modInfoMap[modID].RatingImg = ratingImg
			log.Printf("模组 %s 评分图片: %s", modID, ratingImg)
		}

		mapMutex.Unlock()

		log.Printf("找到模组: %s (ID: %s, 作者: %s)",
			modInfoMap[modID].Name, modID, modInfoMap[modID].Auth)

		// 访问详情页获取更多信息
		detailURL := e.ChildAttr("a[class=ugc]", "href")
		if detailURL != "" {
			// 将模组ID作为上下文传递给详情页爬取
			detailCtx := colly.NewContext()
			detailCtx.Put("modID", modID)
			c.Request("GET", detailURL, nil, detailCtx, nil)
		}
	})

	// 解析模组详细统计信息
	c.OnHTML("div[class=detailsStatsContainerRight]", func(e *colly.HTMLElement) {
		// 从上下文获取模组ID
		modID := e.Request.Ctx.Get("modID")
		if modID == "" {
			return
		}

		mapMutex.Lock()
		defer mapMutex.Unlock()

		if _, exists := modInfoMap[modID]; !exists {
			// 如果模组ID不存在，可能是爬虫直接访问了详情页
			return
		}

		// 获取并设置模组时间信息
		moduptime := e.ChildText("div:nth-child(3)")
		if moduptime != "" {
			modInfoMap[modID].Time = moduptime
			log.Printf("模组 %s 更新时间: %s", modID, moduptime)
		}
	})

	// 解析模组订阅信息
	c.OnHTML("table[class=stats_table]", func(e *colly.HTMLElement) {
		modID := e.Request.Ctx.Get("modID")
		if modID == "" {
			return
		}

		mapMutex.Lock()
		defer mapMutex.Unlock()

		if _, exists := modInfoMap[modID]; !exists {
			return
		}

		// 获取并设置订阅信息
		modnowsub := e.ChildText("tbody>tr:nth-child(2)>td:nth-child(1)")
		if modnowsub != "" {
			modInfoMap[modID].Sub = modnowsub
			log.Printf("模组 %s 订阅数: %s", modID, modnowsub)
		}
	})

	// 解析模组版本信息
	c.OnHTML("div[class=workshopTags]", func(e *colly.HTMLElement) {
		modID := e.Request.Ctx.Get("modID")
		if modID == "" {
			return
		}

		mapMutex.Lock()
		defer mapMutex.Unlock()

		if _, exists := modInfoMap[modID]; !exists {
			return
		}

		// 获取并设置版本信息
		modversion := e.ChildText("a")
		if modversion != "" {
			modInfoMap[modID].Version = modversion
			log.Printf("模组 %s 版本: %s", modID, modversion)
		}
	})

	// 访问搜索页面
	searchURL := fmt.Sprintf(
		"https://steamcommunity.com/workshop/browse/?appid=%s&searchtext=%s&browsesort=trend&section=&actualsort=trend&p=%s&days=-1&numperpage=30",
		appID, keyword, page,
	)

	log.Printf("%s 开始访问搜索页面: %s", LogSearchPrefix, searchURL)
	if err := c.Visit(searchURL); err != nil {
		log.Printf("%s 搜索失败: %v URL: %s", LogSearchPrefix, err, searchURL)
		g.JSON(http.StatusInternalServerError, gin.H{"error": "搜索失败"})
		return
	}

	log.Printf("%s 等待爬取完成...", LogSearchPrefix)
	c.Wait()

	// 按原始顺序构建结果数组
	var searchResults [30]Searchmodinfo
	for i, modID := range modList {
		if i >= 30 {
			log.Printf("%s 结果超过30个，只返回前30个", LogSearchPrefix)
			break // 最多返回30个结果
		}
		if info, exists := modInfoMap[modID]; exists {
			searchResults[i] = *info
		}
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 搜索完成，关键字: %s, 页码: %s, 找到: %d 个模组, 耗时: %v",
		LogSearchPrefix, keyword, page, len(modList), elapsedTime)

	g.JSON(http.StatusOK, searchResults)
}

// DownloadMod 下载模组处理函数
func DownloadMod(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到模组下载请求 来自IP: %s", LogDownPrefix, clientIP)

	modid := g.Query("modid")
	refresh := g.DefaultQuery("refresh", "false")
	version := g.DefaultQuery("version", "")

	// 如果没有从查询参数获取到，则尝试从表单或JSON中获取
	if modid == "" {
		var form Moddown
		if err := g.Bind(&form); err == nil && form.Modid != "" {
			modid = form.Modid
			refresh = form.Refresh
			version = form.Version
			log.Printf("%s 从请求体获取到参数 - 模组ID: %s, 刷新: %s, 版本: %s",
				LogDownPrefix, modid, refresh, version)
		} else if err != nil {
			log.Printf("%s 请求体解析失败: %v", LogDownPrefix, err)
		}
	} else {
		log.Printf("%s 从查询参数获取到参数 - 模组ID: %s, 刷新: %s, 版本: %s",
			LogDownPrefix, modid, refresh, version)
	}

	// 检查是否有模组ID
	if modid == "" {
		log.Printf("%s 未提供模组ID IP: %s", LogDownPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"error": "请提供模组ID"})
		return
	}

	p1 = 0
	status := 401
	log.Printf("%s 开始处理模组下载请求 - 模组ID: %s, 刷新: %s, 版本: %s",
		LogDownPrefix, modid, refresh, version)

	log.Printf("%s 调用DownloadMod2开始下载模组 - 模组ID: %s", LogDownPrefix, modid)
	modinfo := DownloadMod2(modid, refresh, version)

	switch p1 {
	case 0:
		status = 400
		log.Printf("%s 模组下载失败 - 模组ID: %s, 状态码: %d", LogDownPrefix, modid, status)
	case 1:
		status = 200
		log.Printf("%s 模组下载成功 - 模组ID: %s, 状态码: %d", LogDownPrefix, modid, status)
	default:
		status = 401
		modinfo = "异常退出"
		log.Printf("%s 模组下载异常 - 模组ID: %s, 状态码: %d", LogDownPrefix, modid, status)
	}

	// 直接构建JSON字符串，避免嵌套JSON被转义
	data := fmt.Sprintf("{\"status\": %d, \"modinfo\": %s}", status, modinfo)

	elapsedTime := time.Since(startTime)
	log.Printf("%s 模组下载请求处理完成 - 模组ID: %s, 状态码: %d, 耗时: %v",
		LogDownPrefix, modid, status, elapsedTime)

	g.Header("Content-Type", "application/json")
	g.String(http.StatusOK, data)
}

// checktemp 检查模组缓存是否存在
func checktemp(modid string) string {
	log.Printf("%s 检查模组缓存 - 模组ID: %s", LogCachePrefix, modid)
	cachePath := filepath.Join(luaShPath, "temp", modid, "modinfo.lua")
	if info, err := os.Stat(cachePath); err != nil || info.IsDir() {
		log.Printf("%s 模组缓存不存在 - 模组ID: %s, 错误: %v", LogCachePrefix, modid, err)
		return "false"
	}

	log.Printf("%s 模组缓存存在 - 模组ID: %s, 路径: %s", LogCachePrefix, modid, cachePath)
	return "ok"
}

// jsonmod 解析模组信息并转换为JSON格式
func jsonmod(modid string) string {
	log.Printf("%s 开始解析模组信息 - 模组ID: %s", LogPrefix, modid)
	modInfoPath := filepath.Join(workshopContent, modid, "modinfo.lua")
	output, method, err := renderModInfo(modid, modInfoPath)
	if err != nil {
		log.Printf("%s 解析模组信息失败 - 模组ID: %s, 错误: %v", LogPrefix, modid, err)
		return modInfoErrorJSON("解析模组信息失败")
	}

	log.Printf("%s 模组信息解析成功 - 模组ID: %s, 解析器: %s", LogPrefix, modid, method)
	return string(output)
}

// DownloadMod2 处理模组下载的具体实现
func DownloadMod2(modid string, refresh string, version string) string {
	startTime := time.Now()
	log.Printf("%s 开始处理模组下载 - 模组ID: %s, 强制刷新: %s, 版本: %s",
		LogDownPrefix, modid, refresh, version)
	if !isWorkshopModID(modid) {
		p1 = 0
		log.Printf("%s 拒绝无效模组ID: %q", LogDownPrefix, modid)
		return modInfoErrorJSON("模组ID必须为数字")
	}

	// 检查缓存
	if checktemp(modid) == "ok" && refresh != "true" {
		log.Printf("%s 使用缓存的模组信息 - 模组ID: %s", LogCachePrefix, modid)
		cachePath := filepath.Join(luaShPath, "temp", modid, "modinfo.lua")
		parsePath := cachePath
		sourcePath := filepath.Join(workshopContent, modid, "modinfo.lua")
		if info, sourceErr := os.Stat(sourcePath); sourceErr == nil && !info.IsDir() {
			parsePath = sourcePath
		}
		output, method, err := renderModInfo(modid, parsePath)

		if err == nil {
			p1 = 1
			log.Printf("%s 从缓存读取模组信息成功 - 模组ID: %s, 解析器: %s, 耗时: %v",
				LogCachePrefix, modid, method, time.Since(startTime))
			return string(output)
		}

		log.Printf("%s 读取缓存失败 - 模组ID: %s, 错误: %v", LogCachePrefix, modid, err)
	}

	// 检查tmux会话是否存在
	log.Printf("%s 检查tmux会话是否存在 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)
	var isSessionExists bool
	checkCmd := exec.Command("bash", "-c", fmt.Sprintf("tmux has-session -t %s", tmuxSessionName))

	if err := checkCmd.Run(); err != nil {
		isSessionExists = false
		log.Printf("%s tmux会话不存在 - 会话名: %s, 错误: %v", LogTmuxPrefix, tmuxSessionName, err)
	} else {
		isSessionExists = true
		log.Printf("%s tmux会话已存在 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)
	}

	// 如果会话不存在，创建新会话
	if !isSessionExists {
		initCmd := fmt.Sprintf("cd %s && tmux new-session -s %s -d \"./steamcmd.sh\"",
			steamCmdPath, tmuxSessionName)
		log.Printf("%s 创建tmux会话 - 命令: %s", LogTmuxPrefix, initCmd)

		cmd := exec.Command("bash", "-c", initCmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("%s 创建tmux会话失败 - 会话名: %s, 错误: %v, 输出: %s",
				LogTmuxPrefix, tmuxSessionName, err, strings.TrimSpace(string(output)))
			return "创建会话失败"
		}
		log.Printf("%s tmux会话创建成功 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)

		// 设置模组安装路径
		setPathCmd := fmt.Sprintf("tmux send-keys -t %s 'force_install_dir %s' C-m", tmuxSessionName, workshopModPath)
		log.Printf("%s 发送设置模组安装路径命令 - 会话名: %s, 路径: %s", LogTmuxPrefix, tmuxSessionName, workshopModPath)

		cmd = exec.Command("bash", "-c", setPathCmd)
		output, err = cmd.CombinedOutput()
		if err != nil {
			log.Printf("%s 设置模组安装路径失败 - 会话名: %s, 错误: %v, 输出: %s",
				LogTmuxPrefix, tmuxSessionName, err, strings.TrimSpace(string(output)))
			return "设置模组安装路径失败"
		}
		log.Printf("%s 设置模组安装路径命令发送成功 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)

		// 登录Steam
		loginCmd := fmt.Sprintf("tmux send-keys -t %s 'login anonymous' C-m", tmuxSessionName)
		log.Printf("%s 发送Steam匿名登录命令 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)

		cmd = exec.Command("bash", "-c", loginCmd)
		output, err = cmd.CombinedOutput()
		if err != nil {
			log.Printf("%s Steam登录失败 - 会话名: %s, 错误: %v, 输出: %s",
				LogTmuxPrefix, tmuxSessionName, err, strings.TrimSpace(string(output)))
			return "Steam登录失败"
		}
		log.Printf("%s Steam登录命令发送成功 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)
	}

	// 发送下载命令
	downloadCmd := fmt.Sprintf("tmux send-keys -t %s 'workshop_download_item %s %s' C-m",
		tmuxSessionName, appID, modid)
	log.Printf("%s 发送模组下载命令 - 会话名: %s, 模组ID: %s",
		LogTmuxPrefix, tmuxSessionName, modid)

	cmd := exec.Command("bash", "-c", downloadCmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("%s 发送下载命令失败 - 会话名: %s, 模组ID: %s, 错误: %v, 输出: %s",
			LogTmuxPrefix, tmuxSessionName, modid, err, strings.TrimSpace(string(output)))
		return "发送下载命令失败"
	}
	log.Printf("%s 下载命令发送成功 - 会话名: %s, 模组ID: %s",
		LogTmuxPrefix, tmuxSessionName, modid)

	// 创建通道监听下载进度
	log.Printf("%s 开始监听模组下载进度 - 模组ID: %s", LogDownPrefix, modid)
	downloadStatusChan := make(chan string, 10)
	go readmod(modid, downloadStatusChan)

	p1 = 0
	errorResult := ""

	// 处理下载状态
	for statusMsg := range downloadStatusChan {
		log.Printf("%s 模组下载状态 - 模组ID: %s, 状态: %s", LogDownPrefix, modid, statusMsg)

		switch statusMsg {
		case "接收到模组下载请求", "模组正在下载", "接收到模组下载结果信息":
			// 这些是中间状态，继续等待
			continue
		case "下载成功":
			log.Printf("%s 模组下载成功 - 模组ID: %s, 版本: %s",
				LogDownPrefix, modid, version)

			p1 = 1
			close(downloadStatusChan)
			break
		case "下载失败":
			log.Printf("%s 模组下载失败 - 模组ID: %s", LogDownPrefix, modid)
			close(downloadStatusChan)
			break
		default:
			// 其他状态消息可能包含错误信息
			errorResult = statusMsg
			log.Printf("%s 模组下载返回未知状态 - 模组ID: %s, 状态: %s",
				LogDownPrefix, modid, statusMsg)
			close(downloadStatusChan)
			break
		}

		if p1 == 1 {
			log.Printf("%s 模组下载处理完成 - 模组ID: %s, 状态: 成功", LogDownPrefix, modid)
			break
		}
	}

	// 下载失败处理
	if p1 == 0 && errorResult != "" {
		length := len(modid) + 61
		if len(errorResult) > length {
			errorResult = errorResult[length : len(errorResult)-1]
			log.Printf("%s 模组下载失败，返回错误信息 - 模组ID: %s, 错误: %s",
				LogDownPrefix, modid, errorResult)
			return errorResult
		}
		log.Printf("%s 模组下载失败 - 模组ID: %s", LogDownPrefix, modid)
		return "下载失败"
	}

	// 处理下载成功的模组信息
	log.Printf("%s 模组下载成功，开始创建缓存 - 模组ID: %s", LogCachePrefix, modid)
	sourcePath := filepath.Join(workshopContent, modid, "modinfo.lua")
	if cachePath, cacheErr := cacheModInfo(modid, sourcePath); cacheErr != nil {
		log.Printf("%s 创建模组缓存失败 - 模组ID: %s, 错误: %v", LogCachePrefix, modid, cacheErr)
	} else {
		log.Printf("%s 创建模组缓存成功 - 模组ID: %s, 路径: %s", LogCachePrefix, modid, cachePath)
	}

	modInfoOutput, method, parseErr := renderModInfo(modid, sourcePath)
	if parseErr != nil {
		p1 = 0
		log.Printf("%s 模组信息解析失败 - 模组ID: %s, 错误: %v", LogPrefix, modid, parseErr)
		return modInfoErrorJSON("解析模组信息失败")
	}
	log.Printf("%s 模组信息解析成功 - 模组ID: %s, 解析器: %s", LogPrefix, modid, method)
	return string(modInfoOutput)
}

// readmod 监控模组下载进度
func readmod(modid string, ch chan string) {
	downloadComplete := false
	startTime := time.Now()

	// 定义需要监控的日志关键字
	mod1 := "Download item " + modid + " requested by app"
	mod2 := "Starting Workshop download job (requested item " + modid + " )"
	mod3 := "Download item " + modid + " result"

	log.Printf("%s 开始监控模组下载日志 - 模组ID: %s", LogDownPrefix, modid)

	// 配置tail
	tailConfig := tail.Config{
		ReOpen:    true,
		Follow:    true,
		Location:  &tail.SeekInfo{Offset: 0, Whence: 2},
		MustExist: false,
		Poll:      true,
	}

	// 开始tail监控日志文件
	//fileName := steamCmdPath + "/logs/stderr.txt"

	// 确保路径中的空格被正确处理
	cleanPath := strings.ReplaceAll(fileName, "\\", "")
	log.Printf("%s 开始监控日志文件 - 模组ID: %s, 文件路径: %s", LogDownPrefix, modid, cleanPath)

	tails, err := tail.TailFile(cleanPath, tailConfig)
	if err != nil {
		log.Printf("%s 监控日志文件失败 - 模组ID: %s, 错误: %v", LogDownPrefix, modid, err)
		ch <- "下载失败"
		return
	}
	log.Printf("%s 日志文件监控已启动 - 模组ID: %s", LogDownPrefix, modid)

	// 设置超时保护
	timeout := time.After(5 * time.Minute)
	log.Printf("%s 设置下载超时时间为5分钟 - 模组ID: %s", LogDownPrefix, modid)

	// 监控日志
	for {
		select {
		case <-timeout:
			log.Printf("%s 模组下载超时 - 模组ID: %s, 超时时间: 5分钟", LogDownPrefix, modid)
			ch <- "下载失败，超时"
			return
		case line, ok := <-tails.Lines:
			if !ok {
				log.Printf("%s 日志文件关闭，尝试重新打开 - 模组ID: %s, 文件: %s",
					LogDownPrefix, modid, tails.Filename)
				time.Sleep(time.Second)
				continue
			}

			if strings.Contains(line.Text, mod1) {
				log.Printf("%s 检测到模组下载请求 - 模组ID: %s", LogDownPrefix, modid)
				ch <- "接收到模组下载请求"
			} else if strings.Contains(line.Text, mod2) {
				log.Printf("%s 检测到模组开始下载 - 模组ID: %s", LogDownPrefix, modid)
				ch <- "模组正在下载"
			} else if strings.Contains(line.Text, mod3) {
				log.Printf("%s 检测到模组下载结果 - 模组ID: %s", LogDownPrefix, modid)
				ch <- "接收到模组下载结果信息"
				if strings.Contains(line.Text, "result : OK") {
					log.Printf("%s 模组下载成功 - 模组ID: %s, 耗时: %v",
						LogDownPrefix, modid, time.Since(startTime))
					ch <- "下载成功"
					downloadComplete = true
				} else {
					log.Printf("%s 模组下载失败 - 模组ID: %s, 错误日志: %s",
						LogDownPrefix, modid, line.Text)
					ch <- line.Text
					downloadComplete = true
				}
			}

			if downloadComplete {
				log.Printf("%s 模组下载过程完成 - 模组ID: %s, 耗时: %v",
					LogDownPrefix, modid, time.Since(startTime))
				return
			}
		}
	}
}
