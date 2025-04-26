package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

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

func main() {
	// 设置日志前缀
	log.SetPrefix("[TEST] ")

	// 使用固定的存档和世界名称
	archiveName := "TestCluster"
	worldName := "Master"

	// 打印当前工作目录
	cwd, _ := os.Getwd()
	fmt.Printf("当前工作目录: %s\n", cwd)

	// 测试单玩家文件
	testPlayerConfigFile(cwd, archiveName, worldName, "players")

	// 测试多玩家文件
	testPlayerConfigFile(cwd, archiveName, worldName, "players_multi")
}

// testPlayerConfigFile 测试玩家配置文件
func testPlayerConfigFile(cwd, archiveName, worldName, fileName string) {
	// 检查玩家配置文件是否存在
	playerConfigPath := filepath.Join(cwd, "Klei", "DoNotStarveTogether", archiveName, worldName, "save", "mod_config_data", fileName)
	fmt.Printf("\n测试文件: %s\n", fileName)
	fmt.Printf("玩家配置文件路径: %s\n", playerConfigPath)

	if _, err := os.Stat(playerConfigPath); os.IsNotExist(err) {
		fmt.Printf("错误: 玩家配置文件不存在: %s\n", playerConfigPath)
		return
	}

	fmt.Printf("开始测试读取玩家配置文件，存档: %s, 世界: %s\n", archiveName, worldName)

	// 读取文件内容
	file, err := os.Open(playerConfigPath)
	if err != nil {
		fmt.Printf("打开文件失败: %v\n", err)
		return
	}
	defer file.Close()

	// 解析文件内容
	players, err := parsePlayerConfigFile(file)
	if err != nil {
		fmt.Printf("解析文件失败: %v\n", err)
		return
	}

	// 输出结果
	fmt.Printf("解析到 %d 个玩家:\n", len(players))
	for i, player := range players {
		fmt.Printf("%d. ID=%s, 名称='%s' (长度=%d), 管理员=%v, 禁言=%v, 好友=%v, 年龄=%d\n",
			i+1, player.UserID, player.Name, len(player.Name), player.Admin, player.Muted, player.Friend, player.PlayerAge)
	}
}

// parsePlayerConfigFile 解析玩家配置文件
func parsePlayerConfigFile(file *os.File) ([]PlayerConfigInfo, error) {
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
	userIDRe := regexp.MustCompile(`userid="([^"]*)"`)
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
			log.Printf("[TEST] 第 %d 行不匹配玩家信息格式: %s", lineNum, line)
			continue
		}

		// 提取字段内容
		content := matches[1]

		// 尝试提取多个玩家配置
		playerConfigs := playerConfigRe.FindAllStringSubmatch(content, -1)

		// 如果没有找到匹配的玩家配置，尝试将整个内容作为一个玩家的配置
		if len(playerConfigs) == 0 {
			log.Printf("[TEST] 使用整个内容作为一个玩家的配置")
			player := parsePlayerConfig(content, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe)
			if player.UserID != "" {
				players = append(players, player)
			}
		} else {
			// 处理每个玩家配置
			for _, playerConfig := range playerConfigs {
				if len(playerConfig) < 2 {
					continue
				}

				playerContent := playerConfig[1]
				player := parsePlayerConfig(playerContent, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe)

				if player.UserID != "" {
					players = append(players, player)
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

	// 提取各个字段
	userIDMatch := userIDRe.FindStringSubmatch(content)
	nameMatch := nameRe.FindStringSubmatch(content)
	adminMatch := adminRe.FindStringSubmatch(content)
	eventLevelMatch := eventLevelRe.FindStringSubmatch(content)
	mutedMatch := mutedRe.FindStringSubmatch(content)
	friendMatch := friendRe.FindStringSubmatch(content)
	playerAgeMatch := playerAgeRe.FindStringSubmatch(content)

	// 设置字段值
	if len(userIDMatch) >= 2 {
		player.UserID = userIDMatch[1]
	}

	if len(nameMatch) >= 2 {
		// 打印调试信息
		log.Printf("[TEST] 解析到玩家名称: '%s'", nameMatch[1])

		// 打印字节
		log.Printf("[TEST] 玩家名称字节: %v", []byte(nameMatch[1]))

		// 设置玩家名称
		player.Name = nameMatch[1]

		// 打印玩家名称
		log.Printf("[TEST] 设置玩家名称为: '%s', 长度=%d", player.Name, len(player.Name))

		// 打印字节
		log.Printf("[TEST] 设置后玩家名称字节: %v", []byte(player.Name))
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

	return player
}
