package cron

import (
	"bufio"
	"fmt"
	"github.com/go-ini/ini"
	"log"
	"os"
	"path/filepath"
	"regexp"
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
	players, err := parsePlayerConfigFile(file)
	if err != nil {
		return "", fmt.Errorf("解析玩家配置文件失败: %v", err)
	}

	// 构建返回消息
	message := fmt.Sprintf("成功读取玩家配置文件，存档: %s, 世界: %s, 玩家数: %d", archiveName, worldName, len(players))
	log.Printf("[PlayerConfigTask] %s", message)

	// 输出解析结果
	for i, player := range players {
		log.Printf("[PlayerConfigTask] 玩家 %d: ID=%s, 名称=%s, 管理员=%v",
			i+1, player.UserID, player.Name, player.Admin)
	}

	return message, nil
}

// PlayerConfigInfo 玩家配置信息
type PlayerConfigInfo struct {
	UserID     string // 玩家ID
	Name       string // 玩家名称
	Admin      bool   // 是否管理员
	EventLevel int    // 事件等级
	Muted      bool   // 是否被禁言
	Friend     bool   // 是否好友
	PlayerAge  int    // 玩家年龄
}

// parsePlayerConfigFile 解析玩家配置文件
func parsePlayerConfigFile(file *os.File) ([]PlayerConfigInfo, error) {
	var players []PlayerConfigInfo

	scanner := bufio.NewScanner(file)

	// 正则表达式匹配玩家信息
	// 格式: KLEI     1 return {{eventlevel=0,skillselection={0},muted=false,admin=true,userid="KU_HQp7BOVs",friend=false,playerage=0,vanity={},userflags=0,name="[Host]",performance=0,equip={},colour={0.80392156862745,0.30980392156863,0.22352941176471,1},prefab="",lobbycharacter=""}}
	re := regexp.MustCompile(`KLEI\s+\d+\s+return\s+\{\{(.*)\}\}`)

	// 提取字段的正则表达式
	userIDRe := regexp.MustCompile(`userid="([^"]+)"`)
	nameRe := regexp.MustCompile(`name="([^"]+)"`)
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

		// 提取各个字段
		userIDMatch := userIDRe.FindStringSubmatch(content)
		nameMatch := nameRe.FindStringSubmatch(content)
		adminMatch := adminRe.FindStringSubmatch(content)
		eventLevelMatch := eventLevelRe.FindStringSubmatch(content)
		mutedMatch := mutedRe.FindStringSubmatch(content)
		friendMatch := friendRe.FindStringSubmatch(content)
		playerAgeMatch := playerAgeRe.FindStringSubmatch(content)

		// 创建玩家信息对象
		player := PlayerConfigInfo{}

		// 设置字段值
		if len(userIDMatch) >= 2 {
			player.UserID = userIDMatch[1]
		}

		if len(nameMatch) >= 2 {
			player.Name = nameMatch[1]
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

		// 添加到玩家列表
		players = append(players, player)

		log.Printf("[PlayerConfigTask] 解析到玩家: ID=%s, 名称=%s, 管理员=%v",
			player.UserID, player.Name, player.Admin)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("扫描文件失败: %v", err)
	}

	return players, nil
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

// RegisterPlayerConfigTasks 注册玩家配置相关任务
func RegisterPlayerConfigTasks(manager *TaskManager) {
	// 注册读取玩家配置文件任务
	manager.RegisterFunctionWithInfo("read_player_config", ReadPlayerConfigFile, FunctionInfo{
		Name:        "read_player_config",
		Description: "读取玩家配置文件",
		ParamTypes:  []string{"string", "string"}, // 存档名称, 世界名称
	})

	log.Println("[PlayerConfigTask] 玩家配置相关任务注册完成")
}
