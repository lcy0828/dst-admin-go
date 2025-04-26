package main

import (
	"fmt"
	"log"
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

	// 测试数据
	content := `eventlevel=0,skillselection={0},muted=false,admin=true,userid="KU_HQp7BOVs",friend=false,playerage=0,vanity={},userflags=0,name="[Host]",performance=0,equip={},colour={0.80392156862745,0.30980392156863,0.22352941176471,1},prefab="",lobbycharacter=""`

	// 提取字段的正则表达式
	userIDRe := regexp.MustCompile(`userid="([^"]+)"`)
	nameRe := regexp.MustCompile(`name="([^"]*)"`)
	adminRe := regexp.MustCompile(`admin=(true|false)`)
	eventLevelRe := regexp.MustCompile(`eventlevel=(\d+)`)
	mutedRe := regexp.MustCompile(`muted=(true|false)`)
	friendRe := regexp.MustCompile(`friend=(true|false)`)
	playerAgeRe := regexp.MustCompile(`playerage=(\d+)`)

	// 解析玩家配置
	player := parsePlayerConfig(content, userIDRe, nameRe, adminRe, eventLevelRe, mutedRe, friendRe, playerAgeRe)

	// 输出结果
	fmt.Printf("解析结果:\n")
	fmt.Printf("ID=%s, 名称='%s' (长度=%d), 管理员=%v, 禁言=%v, 好友=%v, 年龄=%d\n",
		player.UserID, player.Name, len(player.Name), player.Admin, player.Muted, player.Friend, player.PlayerAge)
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
		log.Printf("解析到玩家名称: '%s'", nameMatch[1])
		// 设置玩家名称
		player.Name = nameMatch[1]
		// 打印玩家名称
		log.Printf("设置玩家名称为: '%s', 长度=%d", player.Name, len(player.Name))
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
