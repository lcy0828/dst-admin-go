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

	// 检查玩家配置文件是否存在
	playerConfigPath := filepath.Join(cwd, "Klei", "DoNotStarveTogether", archiveName, worldName, "save", "mod_config_data", "players")
	fmt.Printf("玩家配置文件路径: %s\n", playerConfigPath)

	if _, err := os.Stat(playerConfigPath); os.IsNotExist(err) {
		fmt.Printf("错误: 玩家配置文件不存在: %s\n", playerConfigPath)
		return
	}

	fmt.Printf("开始测试读取玩家配置文件，存档: %s, 世界: %s\n", archiveName, worldName)

	// 调用函数
	result, err := cron.ReadPlayerConfigFile(archiveName, worldName)
	if err != nil {
		fmt.Printf("测试失败: %v\n", err)
	} else {
		fmt.Printf("测试成功: %s\n", result)
	}
}
