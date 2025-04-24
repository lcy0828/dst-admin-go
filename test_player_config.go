package main

import (
	"dont/cron"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

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
	testSinglePlayerFile(cwd, archiveName, worldName)

	// 测试多玩家文件
	testMultiPlayerFile(cwd, archiveName, worldName)
}

// 测试单玩家文件
func testSinglePlayerFile(cwd, archiveName, worldName string) {
	// 检查玩家配置文件是否存在
	playerConfigPath := filepath.Join(cwd, "Klei", "DoNotStarveTogether", archiveName, worldName, "save", "mod_config_data", "players")
	fmt.Printf("\n测试单玩家文件\n")
	fmt.Printf("玩家配置文件路径: %s\n", playerConfigPath)

	if _, err := os.Stat(playerConfigPath); os.IsNotExist(err) {
		fmt.Printf("错误: 玩家配置文件不存在: %s\n", playerConfigPath)
		return
	}

	fmt.Printf("开始测试读取玩家配置文件，存档: %s, 世界: %s\n", archiveName, worldName)

	// 手动读取文件并解析
	file, err := os.Open(playerConfigPath)
	if err != nil {
		fmt.Printf("打开文件失败: %v\n", err)
		return
	}
	defer file.Close()

	players, err := cron.ParsePlayerConfigFile(file)
	if err != nil {
		fmt.Printf("解析文件失败: %v\n", err)
		return
	}

	fmt.Printf("解析到 %d 个玩家:\n", len(players))
	for i, player := range players {
		fmt.Printf("%d. ID=%s, 名称='%s' (长度=%d), 管理员=%v, 禁言=%v, 好友=%v, 年龄=%d\n",
			i+1, player.UserID, player.Name, len(player.Name), player.Admin, player.Muted, player.Friend, player.PlayerAge)
	}
}

// 测试多玩家文件
func testMultiPlayerFile(cwd, archiveName, worldName string) {
	// 检查玩家配置文件是否存在
	playerConfigPath := filepath.Join(cwd, "Klei", "DoNotStarveTogether", archiveName, worldName, "save", "mod_config_data", "players_multi")
	fmt.Printf("\n测试多玩家文件\n")
	fmt.Printf("玩家配置文件路径: %s\n", playerConfigPath)

	if _, err := os.Stat(playerConfigPath); os.IsNotExist(err) {
		fmt.Printf("错误: 玩家配置文件不存在: %s\n", playerConfigPath)
		return
	}

	fmt.Printf("开始测试读取玩家配置文件，存档: %s, 世界: %s\n", archiveName, worldName)

	// 手动读取文件并解析
	file, err := os.Open(playerConfigPath)
	if err != nil {
		fmt.Printf("打开文件失败: %v\n", err)
		return
	}
	defer file.Close()

	players, err := cron.ParsePlayerConfigFile(file)
	if err != nil {
		fmt.Printf("解析文件失败: %v\n", err)
		return
	}

	fmt.Printf("解析到 %d 个玩家:\n", len(players))
	for i, player := range players {
		fmt.Printf("%d. ID=%s, 名称='%s' (长度=%d), 管理员=%v, 禁言=%v, 好友=%v, 年龄=%d\n",
			i+1, player.UserID, player.Name, len(player.Name), player.Admin, player.Muted, player.Friend, player.PlayerAge)
	}
}
