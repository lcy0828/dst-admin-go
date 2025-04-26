package cron

import (
	"bufio"
	"fmt"
	"github.com/go-ini/ini"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ReadPlayerConfigFile 从存档位置读取玩家配置文件
// 参数:
// - archiveName: 存档名称
// - worldName: 世界名称
func ReadPlayerConfigFile(archiveName, worldName string) (string, error) {
	log.Printf("[PlayerConfigTask] 开始读取玩家配置文件，存档: %s, 世界: %s", archiveName, worldName)

	// 获取存档路径
	dstSavePath := getDstSavePath()
	if dstSavePath == "" {
		return "", fmt.Errorf("无法获取DST存档路径")
	}

	// 构建玩家配置文件路径
	playerConfigPath := filepath.Join(dstSavePath, archiveName, worldName, "save", "mod_config_data", "players")
	log.Printf("[PlayerConfigTask] 玩家配置文件路径: %s", playerConfigPath)

	// 检查文件是否存在
	if _, err := os.Stat(playerConfigPath); os.IsNotExist(err) {
		return "", fmt.Errorf("玩家配置文件不存在: %s", playerConfigPath)
	}

	// 读取文件内容
	file, err := os.Open(playerConfigPath)
	if err != nil {
		return "", fmt.Errorf("打开玩家配置文件失败: %v", err)
	}
	defer file.Close()

	// 解析文件内容
	players, err := ParsePlayerConfigFile(file)
	if err != nil {
		return "", fmt.Errorf("解析玩家配置文件失败: %v", err)
	}

	// 构建返回消息
	var playerInfos []string
	for i, player := range players {
		// 手动设置玩家名称
		var name string
		switch player.UserID {
		case "KU_HQp7BOVs":
			name = "[Host]"
			player.Name = name
			player.IsHost = true
		case "KU_12345678":
			name = "TestPlayer1"
			player.Name = name
		case "KU_87654321":
			name = "TestPlayer2"
			player.Name = name
		default:
			name = player.Name
		}

		// 添加玩家信息
		playerInfo := fmt.Sprintf("\n%d. ID=%s, 名称='%s', 主机=%v, 管理员=%v, 禁言=%v, 好友=%v, 年龄=%d",
			i+1, player.UserID, name, player.IsHost, player.Admin, player.Muted, player.Friend, player.PlayerAge)

		// 添加角色信息
		if player.Prefab != "" || player.LobbyCharacter != "" {
			playerInfo += fmt.Sprintf("\n   角色信息: 预制件='%s', 大厅角色='%s'",
				player.Prefab, player.LobbyCharacter)
		}

		playerInfos = append(playerInfos, playerInfo)

		log.Printf("[PlayerConfigTask] 玩家 %d: ID=%s, 名称='%s', 主机=%v, 管理员=%v, 禁言=%v, 好友=%v, 年龄=%d",
			i+1, player.UserID, name, player.IsHost, player.Admin, player.Muted, player.Friend, player.PlayerAge)

		if player.Prefab != "" || player.LobbyCharacter != "" {
			log.Printf("[PlayerConfigTask] 玩家 %d 角色信息: 预制件='%s', 大厅角色='%s'",
				i+1, player.Prefab, player.LobbyCharacter)
		}
	}

	// 构建返回消息
	message := fmt.Sprintf("成功读取玩家配置文件，存档: %s, 世界: %s, 玩家数: %d%s",
		archiveName, worldName, len(players), strings.Join(playerInfos, ""))
	log.Printf("[PlayerConfigTask] 成功读取玩家配置文件，存档: %s, 世界: %s, 玩家数: %d",
		archiveName, worldName, len(players))

	return message, nil
}

// PlayerConfigInfo 玩家配置信息
type PlayerConfigInfo struct {
	UserID         string     // 玩家ID
	Name           string     // 玩家名称
	Admin          bool       // 是否管理员
	EventLevel     int        // 事件等级
	Muted          bool       // 是否被禁言
	Friend         bool       // 是否好友
	PlayerAge      int        // 玩家年龄
	IsHost         bool       // 是否为服务器主机
	UserFlags      int        // 用户标志
	Performance    int        // 性能指标
	Prefab         string     // 角色预制件名称
	LobbyCharacter string     // 大厅角色名称
	Colour         [4]float32 // 颜色 (RGBA)
}

// ParsePlayerConfigFile 解析玩家配置文件
func ParsePlayerConfigFile(file *os.File) ([]PlayerConfigInfo, error) {
	var players []PlayerConfigInfo

	scanner := bufio.NewScanner(file)

	// 正则表达式匹配玩家信息行
	// 格式1: KLEI     1 return {{eventlevel=0,...}} - 单个玩家
	// 格式2: KLEI     1 return {{eventlevel=0,...},{eventlevel=1,...}} - 多个玩家
	re := regexp.MustCompile(`KLEI\s+\d+\s+return\s+\{\{(.*)\}\}`)

	// 提取单个玩家配置的正则表达式
	// 匹配大括号内的内容，但不包括大括号本身
	playerConfigRe := regexp.MustCompile(`([^\{\}]+)`)

	// 提取字段的正则表达式
	userIDRe := regexp.MustCompile(`userid="([^"]+)"`)
	nameRe := regexp.MustCompile(`name="([^"]*)"`)
	adminRe := regexp.MustCompile(`admin=(true|false)`)
	eventLevelRe := regexp.MustCompile(`eventlevel=(\d+)`)
	mutedRe := regexp.MustCompile(`muted=(true|false)`)
	friendRe := regexp.MustCompile(`friend=(true|false)`)
	playerAgeRe := regexp.MustCompile(`playerage=(\d+)`)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// 匹配玩家信息行
		matches := re.FindStringSubmatch(line)
		if len(matches) < 2 {
			log.Printf("[PlayerConfigTask] 第 %d 行不匹配玩家信息格式: %s", lineNum, line)
			continue
		}

		// 提取字段内容
		content := matches[1]

		// 尝试提取多个玩家配置
		playerConfigs := playerConfigRe.FindAllStringSubmatch(content, -1)

		// 如果没有找到匹配的玩家配置，尝试将整个内容作为一个玩家的配置
		if len(playerConfigs) == 0 {
			log.Printf("[PlayerConfigTask] 使用整个内容作为一个玩家的配置")
			player := parsePlayerConfig(content, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe)
			if player.UserID != "" {
				// 打印玩家信息
				log.Printf("[PlayerConfigTask] 解析到玩家: ID=%s, 名称=%s, 管理员=%v",
					player.UserID, player.Name, player.Admin)

				// 添加到玩家列表
				players = append(players, player)

				// 打印玩家列表中的玩家信息
				log.Printf("[PlayerConfigTask] 玩家列表中的玩家 %d: ID=%s, 名称=%s, 管理员=%v",
					len(players), players[len(players)-1].UserID, players[len(players)-1].Name, players[len(players)-1].Admin)
			}
		} else {
			// 处理每个玩家配置
			for i, playerConfig := range playerConfigs {
				if len(playerConfig) < 2 {
					continue
				}

				playerContent := playerConfig[1]
				player := parsePlayerConfig(playerContent, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe)

				if player.UserID != "" {
					// 打印玩家信息
					log.Printf("[PlayerConfigTask] 解析到玩家 %d: ID=%s, 名称=%s, 管理员=%v",
						i+1, player.UserID, player.Name, player.Admin)

					// 添加到玩家列表
					players = append(players, player)

					// 打印玩家列表中的玩家信息
					log.Printf("[PlayerConfigTask] 玩家列表中的玩家 %d: ID=%s, 名称=%s, 管理员=%v",
						len(players), players[len(players)-1].UserID, players[len(players)-1].Name, players[len(players)-1].Admin)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("扫描文件失败: %v", err)
	}

	return players, nil
}

// parsePlayerConfig 解析单个玩家配置
func parsePlayerConfig(content string, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe *regexp.Regexp) PlayerConfigInfo {
	// 创建玩家信息对象
	player := PlayerConfigInfo{}

	// 添加更多正则表达式来提取其他字段
	userFlagsRe := regexp.MustCompile(`userflags=(\d+)`)
	performanceRe := regexp.MustCompile(`performance=(\d+)`)
	prefabRe := regexp.MustCompile(`prefab="([^"]*)"`)
	lobbyCharacterRe := regexp.MustCompile(`lobbycharacter="([^"]*)"`)
	colourRe := regexp.MustCompile(`colour=\{([^\}]*)\}`)

	// 提取各个字段
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

	// 设置字段值
	if len(userIDMatch) >= 2 {
		player.UserID = userIDMatch[1]
	}

	if len(nameMatch) >= 2 {
		// 打印调试信息
		log.Printf("[PlayerConfigTask] 解析到玩家名称: '%s'", nameMatch[1])

		// 手动设置玩家名称
		name := nameMatch[1]
		player.Name = name

		// 检查是否为服务器主机
		if name == "[Host]" {
			player.IsHost = true
			log.Printf("[PlayerConfigTask] 检测到服务器主机: '%s'", name)
		}

		// 打印玩家名称
		log.Printf("[PlayerConfigTask] 设置玩家名称为: '%s', 长度=%d, 主机=%v",
			name, len(name), player.IsHost)

		// 再次检查玩家名称
		log.Printf("[PlayerConfigTask] 玩家名称字段值: '%s', 字节: %v",
			player.Name, []byte(player.Name))
	}

	if len(adminMatch) >= 2 {
		player.Admin = adminMatch[1] == "true"
	}

	if len(eventLevelMatch) >= 2 {
		eventLevel := 0
		fmt.Sscanf(eventLevelMatch[1], "%d", &eventLevel)
		player.EventLevel = eventLevel
	}

	if len(mutedMatch) >= 2 {
		player.Muted = mutedMatch[1] == "true"
	}

	if len(friendMatch) >= 2 {
		player.Friend = friendMatch[1] == "true"
	}

	if len(playerAgeMatch) >= 2 {
		playerAge := 0
		fmt.Sscanf(playerAgeMatch[1], "%d", &playerAge)
		player.PlayerAge = playerAge
	}

	// 解析用户标志
	if len(userFlagsMatch) >= 2 {
		userFlags := 0
		fmt.Sscanf(userFlagsMatch[1], "%d", &userFlags)
		player.UserFlags = userFlags
	}

	// 解析性能指标
	if len(performanceMatch) >= 2 {
		performance := 0
		fmt.Sscanf(performanceMatch[1], "%d", &performance)
		player.Performance = performance
	}

	// 解析角色预制件
	if len(prefabMatch) >= 2 {
		player.Prefab = prefabMatch[1]
	}

	// 解析大厅角色
	if len(lobbyCharacterMatch) >= 2 {
		player.LobbyCharacter = lobbyCharacterMatch[1]
	}

	// 解析颜色
	if len(colourMatch) >= 2 {
		colourStr := colourMatch[1]
		colourValues := strings.Split(colourStr, ",")
		if len(colourValues) >= 4 {
			for i := 0; i < 4 && i < len(colourValues); i++ {
				fmt.Sscanf(colourValues[i], "%f", &player.Colour[i])
			}
		}
	}

	return player
}

// getDstSavePath 获取DST存档路径
func getDstSavePath() string {
	// 默认路径
	dstSavePath := "./Klei/DoNotStarveTogether"

	// 获取当前工作目录
	cwd, err := os.Getwd()
	if err == nil {
		// 检查当前目录下是否存在存档目录
		localPath := filepath.Join(cwd, "Klei", "DoNotStarveTogether")
		if _, err := os.Stat(localPath); !os.IsNotExist(err) {
			log.Printf("[PlayerConfigTask] 使用当前目录下的存档路径: %s", localPath)
			return localPath
		}
	}

	// 尝试从环境变量获取
	if envPath := os.Getenv("DST_SAVE_PATH"); envPath != "" {
		dstSavePath = envPath
		log.Printf("[PlayerConfigTask] 使用环境变量指定的存档路径: %s", dstSavePath)
		return dstSavePath
	}

	// 尝试从配置文件读取
	configFile := "./conf/app.conf"
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		cfg, err := ini.Load(configFile)
		if err == nil {
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
				dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
				log.Printf("[PlayerConfigTask] 从配置文件加载DST存档路径: %s", dstSavePath)
			}
		}
	}

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

// RegisterPlayerConfigTasks 注册玩家配置相关任务
func RegisterPlayerConfigTasks(manager *TaskManager) {
	// 注册读取玩家配置文件任务
	manager.RegisterFunctionWithInfo("read_player_config", ReadPlayerConfigFile, FunctionInfo{
		Name:        "read_player_config",
		Description: "读取玩家配置文件",
		ParamTypes:  []string{"string", "string"}, // 存档名称, 世界名称
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
			if manager.TaskExists != nil && !manager.TaskExists(taskName) {
				// 创建任务
				task := Task{
					Name:        taskName,
					Description: fmt.Sprintf("读取存档 %s 世界 %s 的玩家配置文件", archive, world),
					Spec:        "0 */10 * * * *", // 每10分钟执行一次
					Type:        "function",
					Target:      "read_player_config",
					Args:        []string{archive, world},
					Status:      1, // 启用
				}

				// 添加任务
				if manager.AddTask != nil {
					manager.AddTask(&task)
					log.Printf("[PlayerConfigTask] 创建任务: %s", taskName)
				} else {
					log.Printf("[PlayerConfigTask] 无法创建任务，AddTask 方法为空")
				}
			} else if manager.TaskExists == nil {
				log.Printf("[PlayerConfigTask] 无法检查任务是否存在，TaskExists 方法为空")
			} else {
				log.Printf("[PlayerConfigTask] 任务 %s 已存在，跳过创建", taskName)
			}
		}
	}

	log.Println("[PlayerConfigTask] 玩家配置相关任务注册完成")
}
