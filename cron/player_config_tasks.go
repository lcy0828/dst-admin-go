package cron

import (
	"crypto/md5"
	"dont/models"
	"dont/pkg/configpath"
	"dont/pkg/types"
	"encoding/hex"
	"fmt"
	"github.com/go-ini/ini"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// fileCache 文件缓存结构，用于存储文件的最后修改时间和内容哈希
type fileCache struct {
	ModTime     time.Time // 文件最后修改时间
	ContentMD5  string    // 文件内容的MD5哈希值
	PlayerCount int       // 玩家数量
	FileSize    int64     // 文件大小
}

// 全局文件缓存映射，键为文件路径
var (
	fileCacheMap   = make(map[string]fileCache)
	fileCacheMutex sync.RWMutex

	// 缓存DST存档路径，避免重复读取
	cachedDstSavePath      string
	dstSavePathInitialized bool
)

// calculateFileMD5 计算文件的MD5哈希值
func calculateFileMD5(filePath string) (string, error) {
	// 读取文件内容
	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %v", err)
	}

	// 计算MD5哈希值
	hash := md5.Sum(fileContent)
	// 转换为十六进制字符串
	md5Str := hex.EncodeToString(hash[:])

	return md5Str, nil
}

// ReadPlayerConfigFile 从存档位置读取玩家配置文件
// 参数:
// - archiveName: 存档名称
// - worldName: 世界名称
// - filterMode: 过滤模式，0=全部显示，1=只显示主机，2=只显示玩家
func ReadPlayerConfigFile(archiveName, worldName string, filterMode ...int) (string, error) {
	// 默认显示全部
	mode := 0 // 0=全部显示，1=只显示主机，2=只显示玩家
	if len(filterMode) > 0 && filterMode[0] >= 0 && filterMode[0] <= 2 {
		mode = filterMode[0]
	}

	// 模式描述不再需要，因为我们简化了日志输出
	// var modeDesc string
	// switch mode {
	// case 0:
	// 	modeDesc = "全部"
	// case 1:
	// 	modeDesc = "只显示主机"
	// case 2:
	// 	modeDesc = "只显示玩家"
	// }

	// 获取存档路径
	dstSavePath := getDstSavePath()
	if dstSavePath == "" {
		return "", fmt.Errorf("无法获取DST存档路径")
	}

	// 构建玩家配置文件路径
	playerConfigPath := filepath.Join(dstSavePath, archiveName, worldName, "save", "mod_config_data", "players")

	// 检查文件是否存在
	fileInfo, err := os.Stat(playerConfigPath)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("玩家配置文件不存在: %s", playerConfigPath)
	} else if err != nil {
		return "", fmt.Errorf("获取文件信息失败: %v", err)
	}

	// 获取文件修改时间和大小
	modTime := fileInfo.ModTime()
	fileSize := fileInfo.Size()

	// 检查文件是否被修改
	fileCacheMutex.RLock()
	cache, exists := fileCacheMap[playerConfigPath]
	fileCacheMutex.RUnlock()

	if exists {
		// 如果文件大小和修改时间都没变，直接返回缓存的结果
		if cache.FileSize == fileSize && cache.ModTime.Equal(modTime) {
			// 构建返回消息，使用缓存的玩家数量
			message := fmt.Sprintf("文件未变化，使用缓存结果。存档: %s, 世界: %s, 玩家数: %d",
				archiveName, worldName, cache.PlayerCount)
			return message, nil
		}

		// 如果只有修改时间变了但大小没变，计算MD5确认内容是否真的变了
		if cache.FileSize == fileSize && !cache.ModTime.Equal(modTime) {
			// 计算文件MD5
			md5Str, err := calculateFileMD5(playerConfigPath)
			if err == nil && md5Str == cache.ContentMD5 {
				// 内容没变，只更新缓存中的修改时间
				fileCacheMutex.Lock()
				cache.ModTime = modTime
				fileCacheMap[playerConfigPath] = cache
				fileCacheMutex.Unlock()

				// 构建返回消息，使用缓存的玩家数量
				message := fmt.Sprintf("文件内容未变化，使用缓存结果。存档: %s, 世界: %s, 玩家数: %d",
					archiveName, worldName, cache.PlayerCount)
				return message, nil
			}
		}
	}

	// 只在文件有变化时记录日志
	log.Printf("[PlayerConfigTask] 读取玩家配置文件，存档: %s, 世界: %s", archiveName, worldName)

	// 读取文件内容
	fileContent, err := os.ReadFile(playerConfigPath)
	if err != nil {
		return "", fmt.Errorf("读取玩家配置文件失败: %v", err)
	}

	// 计算文件MD5
	md5Str, err := calculateFileMD5(playerConfigPath)
	if err != nil {
		// 减少不必要的日志输出，只在调试时输出
		// log.Printf("[PlayerConfigTask] 计算文件MD5失败: %v", err)
		// 继续处理，不中断流程
	}

	// 解析文件内容
	players, err := ParsePlayerConfigString(string(fileContent))
	if err != nil {
		return "", fmt.Errorf("解析玩家配置文件失败: %v", err)
	}

	// 将玩家配置信息保存到数据库
	if err := models.SavePlayerConfigInfo(archiveName, worldName, players); err != nil {
		log.Printf("[PlayerConfigTask] 保存玩家配置信息到数据库失败: %v", err)
		// 不返回错误，继续处理
	}

	// 更新文件缓存
	fileCacheMutex.Lock()
	fileCacheMap[playerConfigPath] = fileCache{
		ModTime:     modTime,
		ContentMD5:  md5Str,
		PlayerCount: len(players),
		FileSize:    fileSize,
	}
	fileCacheMutex.Unlock()

	// 根据过滤模式过滤玩家列表
	switch mode {
	case 1: // 只显示主机
		var filteredPlayers []PlayerConfigInfo
		for _, player := range players {
			if player.IsHost {
				filteredPlayers = append(filteredPlayers, player)
			}
		}
		players = filteredPlayers
	case 2: // 只显示玩家
		var filteredPlayers []PlayerConfigInfo
		for _, player := range players {
			if !player.IsHost {
				filteredPlayers = append(filteredPlayers, player)
			}
		}
		players = filteredPlayers
	}

	// 构建返回消息
	var playerInfos []string
	for i, player := range players {
		// 添加玩家信息
		playerInfo := fmt.Sprintf("\n%d. ID=%s, 名称='%s', 主机=%v, 管理员=%v, 禁言=%v, 好友=%v, 年龄=%d",
			i+1, player.UserID, player.Name, player.IsHost, player.Admin, player.Muted, player.Friend, player.PlayerAge)

		// 添加角色信息
		if player.Prefab != "" || player.LobbyCharacter != "" {
			playerInfo += fmt.Sprintf("\n   角色信息: 预制件='%s', 大厅角色='%s'",
				player.Prefab, player.LobbyCharacter)
		}

		// 添加基础皮肤信息
		if player.BaseSkin != "" {
			playerInfo += fmt.Sprintf("\n   皮肤信息: 基础皮肤='%s'", player.BaseSkin)
		}

		// 添加网络信息
		if player.NetID != "" {
			playerInfo += fmt.Sprintf("\n   网络信息: NetID='%s', NetScore=%d", player.NetID, player.NetScore)
		}

		// 添加颜色信息
		if player.Colour[0] != 0 || player.Colour[1] != 0 || player.Colour[2] != 0 || player.Colour[3] != 0 {
			playerInfo += fmt.Sprintf("\n   颜色信息: RGBA=[%.2f, %.2f, %.2f, %.2f]", player.Colour[0], player.Colour[1], player.Colour[2], player.Colour[3])
		}

		playerInfos = append(playerInfos, playerInfo)
	}

	// 构建返回消息
	message := fmt.Sprintf("成功读取玩家配置文件，存档: %s, 世界: %s, 玩家数: %d%s",
		archiveName, worldName, len(players), strings.Join(playerInfos, ""))
	// 简化日志输出
	log.Printf("[PlayerConfigTask] 读取完成: %s/%s, 玩家数: %d", archiveName, worldName, len(players))

	return message, nil
}

// PlayerConfigInfo 玩家配置信息类型别名，使用共享类型
type PlayerConfigInfo = types.PlayerConfigInfo

// parsePlayerConfig 解析单个玩家配置
func parsePlayerConfig(content string, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe *regexp.Regexp) PlayerConfigInfo {
	// 创建玩家信息对象
	player := PlayerConfigInfo{
		Colour:         [4]float32{0, 0, 0, 1},
		Vanity:         make(map[string]string),
		Equip:          make(map[string]string),
		SkillSelection: []int{},
	}

	// 清理内容，去除可能的前后空格
	content = strings.TrimSpace(content)

	// 添加更多正则表达式来提取其他字段
	userFlagsRe := regexp.MustCompile(`userflags=(\d+)`)
	performanceRe := regexp.MustCompile(`performance=(\d+)`)
	prefabRe := regexp.MustCompile(`prefab="([^"]*)"`)
	lobbyCharacterRe := regexp.MustCompile(`lobbycharacter="([^"]*)"`)
	colourRe := regexp.MustCompile(`colour=\{([^\}]*)\}`)
	baseSkinRe := regexp.MustCompile(`base_skin="([^"]*)"`)
	netIDRe := regexp.MustCompile(`netid="([^"]*)"`)
	netScoreRe := regexp.MustCompile(`netscore=(\d+)`)
	skillSelectionRe := regexp.MustCompile(`skillselection=\{([^\}]*)\}`)
	vanityRe := regexp.MustCompile(`vanity=\{([^\}]*)\}`)
	equipRe := regexp.MustCompile(`equip=\{([^\}]*)\}`)

	// 将内容按字段分割，以便更好地匹配
	// 注意：这里的分割需要小心处理，因为有些字段内部也有逗号，如colour={0.8,0.3,0.2,1}
	// 所以我们使用正则表达式直接匹配完整字段
	// 移除输出整个配置内容的日志，减少日志量
	// log.Printf("[PlayerConfigTask] 解析玩家配置内容: %s", content)

	// 使用正则表达式提取各个字段
	userIDMatch := userIDRe.FindStringSubmatch(content)
	nameMatch := nameRe.FindStringSubmatch(content)
	adminMatch := adminRe.FindStringSubmatch(content)
	eventLevelMatch := eventLevelRe.FindStringSubmatch(content)
	mutedMatch := mutedRe.FindStringSubmatch(content)
	friendMatch := friendRe.FindStringSubmatch(content)
	playerAgeMatch := playerAgeRe.FindStringSubmatch(content)
	userFlagsMatch := userFlagsRe.FindStringSubmatch(content)
	performanceMatch := performanceRe.FindStringSubmatch(content)
	prefabMatch := prefabRe.FindStringSubmatch(content)
	lobbyCharacterMatch := lobbyCharacterRe.FindStringSubmatch(content)
	colourMatch := colourRe.FindStringSubmatch(content)
	baseSkinMatch := baseSkinRe.FindStringSubmatch(content)
	netIDMatch := netIDRe.FindStringSubmatch(content)
	netScoreMatch := netScoreRe.FindStringSubmatch(content)
	skillSelectionMatch := skillSelectionRe.FindStringSubmatch(content)
	vanityMatch := vanityRe.FindStringSubmatch(content)
	equipMatch := equipRe.FindStringSubmatch(content)

	// 设置字段值
	if len(userIDMatch) >= 2 {
		player.UserID = userIDMatch[1]
		//log.Printf("[PlayerConfigTask] 解析到用户ID: %s", player.UserID)
	}

	if len(nameMatch) >= 2 {
		player.Name = nameMatch[1]
		//log.Printf("[PlayerConfigTask] 解析到玩家名称: '%s'", player.Name)

		// 检查是否为服务器主机
		if player.Name == "[Host]" {
			player.IsHost = true
			//log.Printf("[PlayerConfigTask] 检测到服务器主机: '%s'", player.Name)
		}
	}

	if len(adminMatch) >= 2 {
		player.Admin = adminMatch[1] == "true"
		//log.Printf("[PlayerConfigTask] 解析到管理员状态: %v", player.Admin)
	}

	if len(eventLevelMatch) >= 2 {
		eventLevel := 0
		fmt.Sscanf(eventLevelMatch[1], "%d", &eventLevel)
		player.EventLevel = eventLevel
		//log.Printf("[PlayerConfigTask] 解析到事件等级: %d", player.EventLevel)
	}

	if len(mutedMatch) >= 2 {
		player.Muted = mutedMatch[1] == "true"
		//log.Printf("[PlayerConfigTask] 解析到禁言状态: %v", player.Muted)
	}

	if len(friendMatch) >= 2 {
		player.Friend = friendMatch[1] == "true"
		//log.Printf("[PlayerConfigTask] 解析到好友状态: %v", player.Friend)
	}

	if len(playerAgeMatch) >= 2 {
		playerAge := 0
		fmt.Sscanf(playerAgeMatch[1], "%d", &playerAge)
		player.PlayerAge = playerAge
		//log.Printf("[PlayerConfigTask] 解析到玩家年龄: %d", player.PlayerAge)
	}

	// 解析用户标志
	if len(userFlagsMatch) >= 2 {
		userFlags := 0
		fmt.Sscanf(userFlagsMatch[1], "%d", &userFlags)
		player.UserFlags = userFlags
		//log.Printf("[PlayerConfigTask] 解析到用户标志: %d", player.UserFlags)
	}

	// 解析性能指标
	if len(performanceMatch) >= 2 {
		performance := 0
		fmt.Sscanf(performanceMatch[1], "%d", &performance)
		player.Performance = performance
		//log.Printf("[PlayerConfigTask] 解析到性能指标: %d", player.Performance)
	}

	// 解析角色预制件
	if len(prefabMatch) >= 2 {
		player.Prefab = prefabMatch[1]
		//log.Printf("[PlayerConfigTask] 解析到角色预制件: %s", player.Prefab)
	}

	// 解析大厅角色
	if len(lobbyCharacterMatch) >= 2 {
		player.LobbyCharacter = lobbyCharacterMatch[1]
		//log.Printf("[PlayerConfigTask] 解析到大厅角色: %s", player.LobbyCharacter)
	}

	// 解析颜色
	if len(colourMatch) >= 2 {
		colourStr := colourMatch[1]
		colourValues := strings.Split(colourStr, ",")
		if len(colourValues) >= 4 {
			for i := 0; i < 4 && i < len(colourValues); i++ {
				fmt.Sscanf(strings.TrimSpace(colourValues[i]), "%f", &player.Colour[i])
			}
			//log.Printf("[PlayerConfigTask] 解析到颜色: [%.2f, %.2f, %.2f, %.2f]",
			//player.Colour[0], player.Colour[1], player.Colour[2], player.Colour[3])
		}
	}

	// 解析基础皮肤
	if len(baseSkinMatch) >= 2 {
		player.BaseSkin = baseSkinMatch[1]
		//log.Printf("[PlayerConfigTask] 解析到基础皮肤: %s", player.BaseSkin)
	}

	// 解析网络服务标识符
	if len(netIDMatch) >= 2 {
		player.NetID = netIDMatch[1]
		//log.Printf("[PlayerConfigTask] 解析到网络服务标识符: %s", player.NetID)
	}

	// 解析网络评分
	if len(netScoreMatch) >= 2 {
		netScore := 0
		fmt.Sscanf(netScoreMatch[1], "%d", &netScore)
		player.NetScore = netScore
		//log.Printf("[PlayerConfigTask] 解析到网络评分: %d", player.NetScore)
	}

	// 解析技能选择
	if len(skillSelectionMatch) >= 2 {
		skillStr := skillSelectionMatch[1]
		skillValues := strings.Split(skillStr, ",")
		for _, skill := range skillValues {
			skillValue := 0
			fmt.Sscanf(strings.TrimSpace(skill), "%d", &skillValue)
			player.SkillSelection = append(player.SkillSelection, skillValue)
		}
		//log.Printf("[PlayerConfigTask] 解析到技能选择: %v", player.SkillSelection)
	}

	// 解析装饰性物品
	if len(vanityMatch) >= 2 {
		vanityStr := vanityMatch[1]
		if vanityStr != "" {
			player.Vanity["raw"] = vanityStr
			//log.Printf("[PlayerConfigTask] 解析到装饰性物品: %s", vanityStr)
		}
	}

	// 解析装备物品
	if len(equipMatch) >= 2 {
		equipStr := equipMatch[1]
		if equipStr != "" {
			player.Equip["raw"] = equipStr
			//log.Printf("[PlayerConfigTask] 解析到装备物品: %s", equipStr)
		}
	}

	// 添加一个简洁的摘要日志，只包含关键信息
	if player.UserID != "" {
		// 构建简洁的玩家信息描述
		playerType := "玩家"
		if player.IsHost {
			playerType = "主机"
		}
		adminStatus := ""
		if player.Admin {
			adminStatus = "[管理员]"
		}

		// 输出简洁的日志
		log.Printf("[PlayerConfigTask] 解析到%s: ID=%s, 名称='%s'%s",
			playerType, player.UserID, player.Name, adminStatus)
	}

	return player
}

// getDstSavePath 获取DST存档路径
func getDstSavePath() string {
	// 默认路径
	dstSavePath := "./Klei/DoNotStarveTogether"

	// 使用包级变量缓存路径，避免重复读取
	// Go 不支持静态局部变量，所以我们使用包级变量
	if dstSavePathInitialized {
		return cachedDstSavePath
	}

	// 首先尝试从配置文件读取
	configFile := configpath.Current()
	// 减少日志输出，只在首次读取时输出
	// log.Printf("[PlayerConfigTask] 尝试读取配置文件: %s", configFile)

	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		// log.Printf("[PlayerConfigTask] 配置文件存在")
		if cfg, err := ini.Load(configFile); err == nil {
			// log.Printf("[PlayerConfigTask] 成功加载配置文件")
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
				dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
				// 只在首次读取时输出日志
				log.Printf("[PlayerConfigTask] 从配置文件加载DST存档路径: %s", dstSavePath)
				// 缓存路径
				cachedDstSavePath = dstSavePath
				dstSavePathInitialized = true
				return dstSavePath
			} else {
				// log.Printf("[PlayerConfigTask] 配置文件中没有 DST_SAVE_PATH 配置")
			}
		} else {
			// 只在出错时输出日志
			log.Printf("[PlayerConfigTask] 加载配置文件失败: %v", err)
		}
	} else {
		// log.Printf("[PlayerConfigTask] 配置文件不存在: %s", configFile)
	}

	// 如果配置文件不存在或者没有配置，尝试使用环境变量
	if envPath := os.Getenv("DST_SAVE_PATH"); envPath != "" {
		dstSavePath = envPath
		log.Printf("[PlayerConfigTask] 从环境变量加载DST存档路径: %s", dstSavePath)
		// 缓存路径
		cachedDstSavePath = dstSavePath
		dstSavePathInitialized = true
		return dstSavePath
	}

	// 如果环境变量也没有设置，尝试使用当前目录
	cwd, err := os.Getwd()
	if err == nil {
		// 检查当前目录下是否存在存档目录
		localPath := filepath.Join(cwd, "Klei", "DoNotStarveTogether")
		if _, err := os.Stat(localPath); !os.IsNotExist(err) {
			log.Printf("[PlayerConfigTask] 使用当前目录下的存档路径: %s", localPath)
			// 缓存路径
			cachedDstSavePath = localPath
			dstSavePathInitialized = true
			return localPath
		}
	}

	// 如果所有方法都失败，使用默认路径
	log.Printf("[PlayerConfigTask] 使用默认存档路径: %s", dstSavePath)
	// 缓存路径
	cachedDstSavePath = dstSavePath
	dstSavePathInitialized = true
	return dstSavePath
}

// GetArchiveList 获取存档列表
func GetArchiveList() []string {
	var archives []string

	// 获取存档路径
	dstSavePath := getDstSavePath()

	// 读取目录
	entries, err := os.ReadDir(dstSavePath)
	if err != nil {
		log.Printf("[PlayerConfigTask] 无法读取存档目录: %v", err)
		return archives
	}

	// 遍历目录
	for _, entry := range entries {
		if entry.IsDir() {
			// 检查是否为存档目录
			archivePath := filepath.Join(dstSavePath, entry.Name())
			if isArchiveDir(archivePath) {
				archives = append(archives, entry.Name())
			}
		}
	}

	return archives
}

// GetWorldList 获取世界列表
func GetWorldList(archive string) []string {
	var worlds []string

	// 获取存档路径
	dstSavePath := getDstSavePath()

	// 构建存档路径
	archivePath := filepath.Join(dstSavePath, archive)

	// 读取目录
	entries, err := os.ReadDir(archivePath)
	if err != nil {
		log.Printf("[PlayerConfigTask] 无法读取世界目录: %v", err)
		return worlds
	}

	// 遍历目录
	for _, entry := range entries {
		if entry.IsDir() {
			// 检查是否为世界目录
			worldPath := filepath.Join(archivePath, entry.Name())
			if isWorldDir(worldPath) {
				worlds = append(worlds, entry.Name())
			}
		}
	}

	return worlds
}

// isArchiveDir 检查是否为存档目录
func isArchiveDir(path string) bool {
	// 检查是否存在 server.ini 文件
	serverIniPath := filepath.Join(path, "server.ini")
	if _, err := os.Stat(serverIniPath); !os.IsNotExist(err) {
		return true
	}

	// 检查是否存在 Master 或 Caves 目录
	masterPath := filepath.Join(path, "Master")
	cavesPath := filepath.Join(path, "Caves")

	if _, err := os.Stat(masterPath); !os.IsNotExist(err) {
		return true
	}

	if _, err := os.Stat(cavesPath); !os.IsNotExist(err) {
		return true
	}

	return false
}

// isWorldDir 检查是否为世界目录
func isWorldDir(path string) bool {
	// 检查是否存在 save 目录
	savePath := filepath.Join(path, "save")
	if _, err := os.Stat(savePath); !os.IsNotExist(err) {
		return true
	}

	// 检查是否存在 server.ini 文件
	serverIniPath := filepath.Join(path, "server.ini")
	if _, err := os.Stat(serverIniPath); !os.IsNotExist(err) {
		return true
	}

	return false
}

// ParsePlayerConfigString 解析玩家配置字符串
func ParsePlayerConfigString(content string) ([]PlayerConfigInfo, error) {
	var players []PlayerConfigInfo

	// 正则表达式匹配玩家信息行
	// 格式1: KLEI     1 return {{eventlevel=0,...}} - 单个玩家
	// 格式2: KLEI     1 return {{eventlevel=0,...},{eventlevel=1,...}} - 多个玩家
	// 注意：有些文件可能没有KLEI前缀，直接是玩家数据
	re := regexp.MustCompile(`(?:KLEI\s+\d+\s+return\s+)?\{(.*)\}`)

	// 匹配玩家信息行
	matches := re.FindStringSubmatch(content)
	if len(matches) < 2 {
		log.Printf("[PlayerConfigTask] 内容不匹配玩家信息格式: %s", content)
		return players, nil
	}

	// 提取字段内容
	content = matches[1]

	// 提取单个玩家配置的正则表达式
	// 匹配大括号内的内容，但不包括大括号本身
	// 使用平衡组匹配法来处理嵌套的大括号
	// 这个正则表达式能够处理嵌套的大括号结构，如colour={0.8,0.3,0.2,1}
	playerConfigRe := regexp.MustCompile(`\{([^\{\}]*(\{[^\{\}]*\}[^\{\}]*)*)\}`)

	// 提取字段的正则表达式
	userIDRe := regexp.MustCompile(`userid="([^"]+)"`)
	nameRe := regexp.MustCompile(`name="([^"]*)"`)
	adminRe := regexp.MustCompile(`admin=(true|false)`)
	eventLevelRe := regexp.MustCompile(`eventlevel=(\d+)`)
	mutedRe := regexp.MustCompile(`muted=(true|false)`)
	friendRe := regexp.MustCompile(`friend=(true|false)`)
	playerAgeRe := regexp.MustCompile(`playerage=(\d+)`)

	// 移除输出原始内容的日志，减少日志量
	// log.Printf("[PlayerConfigTask] 原始内容: %s", content)

	// 尝试提取多个玩家配置
	playerConfigs := playerConfigRe.FindAllStringSubmatch(content, -1)
	// 简化日志输出
	if len(playerConfigs) > 0 {
		log.Printf("[PlayerConfigTask] 找到 %d 个玩家配置", len(playerConfigs))
	}

	// 移除输出每个玩家配置内容的日志
	// for i, config := range playerConfigs {
	// 	if len(config) >= 2 {
	// 		log.Printf("[PlayerConfigTask] 玩家配置 %d: %s", i+1, config[1])
	// 	}
	// }

	// 如果没有找到匹配的玩家配置，尝试将整个内容作为一个玩家的配置
	if len(playerConfigs) == 0 {
		log.Printf("[PlayerConfigTask] 使用整个内容作为一个玩家的配置")
		player := parsePlayerConfig(content, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe)
		if player.UserID != "" {
			// 玩家信息已在 parsePlayerConfig 函数中输出，这里不需要重复输出
			// log.Printf("[PlayerConfigTask] 解析到玩家: ID=%s, 名称='%s', 管理员=%v",
			// 	player.UserID, player.Name, player.Admin)

			// 添加到玩家列表
			players = append(players, player)
		}
	} else {
		// 处理每个玩家配置
		for _, playerConfig := range playerConfigs {
			if len(playerConfig) < 2 {
				continue
			}

			playerContent := playerConfig[1]
			// 移除输出玩家配置内容的日志
			// log.Printf("[PlayerConfigTask] 解析玩家 %d 的配置: %s", i+1, playerContent)
			player := parsePlayerConfig(playerContent, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe)

			if player.UserID != "" {
				// 玩家信息已在 parsePlayerConfig 函数中输出，这里不需要重复输出
				// log.Printf("[PlayerConfigTask] 解析到玩家 %d: ID=%s, 名称='%s', 管理员=%v, 主机=%v",
				// 	i+1, player.UserID, player.Name, player.Admin, player.IsHost)

				// 添加到玩家列表
				players = append(players, player)
			}
		}
	}

	return players, nil
}

// TestParsePlayerConfig 测试解析玩家配置
func TestParsePlayerConfig(content string) string {
	players, err := ParsePlayerConfigString(content)
	if err != nil {
		return fmt.Sprintf("解析失败: %v", err)
	}

	// 构建返回消息
	var playerInfos []string
	for i, player := range players {
		// 添加玩家信息
		playerInfo := fmt.Sprintf("\n%d. ID=%s, 名称='%s', 主机=%v, 管理员=%v, 禁言=%v, 好友=%v, 年龄=%d",
			i+1, player.UserID, player.Name, player.IsHost, player.Admin, player.Muted, player.Friend, player.PlayerAge)

		// 添加角色信息
		if player.Prefab != "" || player.LobbyCharacter != "" {
			playerInfo += fmt.Sprintf("\n   角色信息: 预制件='%s', 大厅角色='%s'",
				player.Prefab, player.LobbyCharacter)
		}

		// 添加基础皮肤信息
		if player.BaseSkin != "" {
			playerInfo += fmt.Sprintf("\n   皮肤信息: 基础皮肤='%s'", player.BaseSkin)
		}

		// 添加网络信息
		if player.NetID != "" {
			playerInfo += fmt.Sprintf("\n   网络信息: NetID='%s', NetScore=%d", player.NetID, player.NetScore)
		}

		// 添加颜色信息
		if player.Colour[0] != 0 || player.Colour[1] != 0 || player.Colour[2] != 0 || player.Colour[3] != 0 {
			playerInfo += fmt.Sprintf("\n   颜色信息: RGBA=[%.2f, %.2f, %.2f, %.2f]", player.Colour[0], player.Colour[1], player.Colour[2], player.Colour[3])
		}

		playerInfos = append(playerInfos, playerInfo)
	}

	// 构建返回消息
	message := fmt.Sprintf("成功解析玩家配置，玩家数: %d%s",
		len(players), strings.Join(playerInfos, ""))

	return message
}

// RegisterPlayerConfigTasks 注册玩家配置相关任务
func RegisterPlayerConfigTasks(manager *TaskManager) {
	// 注册读取玩家配置文件任务
	manager.RegisterFunctionWithInfo("read_player_config", ReadPlayerConfigFile, FunctionInfo{
		Name:        "read_player_config",
		Description: "读取玩家配置文件",
		ParamTypes:  []string{"string", "string", "int?"}, // 存档名称, 世界名称, 过滤模式(可选)
	})

	// 注册测试解析玩家配置函数
	manager.RegisterFunctionWithInfo("test_parse_player_config", TestParsePlayerConfig, FunctionInfo{
		Name:        "test_parse_player_config",
		Description: "测试解析玩家配置字符串",
		ParamTypes:  []string{"string"}, // 配置字符串
	})

	// 创建默认的玩家配置读取任务
	archives := GetArchiveList()
	log.Printf("[PlayerConfigTask] 找到 %d 个存档", len(archives))

	for _, archive := range archives {
		worlds := GetWorldList(archive)
		log.Printf("[PlayerConfigTask] 存档 %s 中找到 %d 个世界", archive, len(worlds))

		for _, world := range worlds {
			// 创建任务名称
			taskName := fmt.Sprintf("read_player_config_%s_%s", archive, world)

			// 检查玩家配置文件是否存在
			dstSavePath := getDstSavePath()
			playerConfigPath := filepath.Join(dstSavePath, archive, world, "save", "mod_config_data", "players")

			if _, err := os.Stat(playerConfigPath); os.IsNotExist(err) {
				log.Printf("[PlayerConfigTask] 存档 %s 世界 %s 的玩家配置文件不存在，跳过创建任务", archive, world)
				continue
			}

			// 检查任务是否已存在
			// 使用数据库查询检查任务是否存在
			tasks, err := models.GetAllTasks()
			taskExists := false
			if err == nil {
				for _, t := range tasks {
					if t.Name == taskName {
						taskExists = true
						break
					}
				}
			}

			if !taskExists {
				// 创建任务
				task := &models.CronTask{
					Name:        taskName,
					Description: fmt.Sprintf("读取存档 %s 世界 %s 的玩家配置文件", archive, world),
					Spec:        "*/5 * * * * *", // 每5秒执行一次
					Type:        "function",
					Target:      "read_player_config",
					Status:      1, // 启用
					CreatedAt:   time.Now(),
					UpdatedAt:   time.Now(),
				}

				// 设置参数
				if err := task.SetArgs([]interface{}{archive, world}); err != nil {
					log.Printf("[PlayerConfigTask] 设置任务参数失败: %v", err)
				}

				// 添加任务
				if err := manager.AddTask(task); err != nil {
					log.Printf("[PlayerConfigTask] 创建任务失败: %v", err)
				} else {
					log.Printf("[PlayerConfigTask] 创建任务成功: %s", taskName)
				}
			} else {
				log.Printf("[PlayerConfigTask] 任务 %s 已存在，跳过创建", taskName)
			}
		}
	}

	log.Println("[PlayerConfigTask] 玩家配置相关任务注册完成")
}
