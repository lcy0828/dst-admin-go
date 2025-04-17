package models

import (
	"fmt"
	"log"
)

// InitCronTables 初始化所有与定时任务相关的表
func InitCronTables() {
	// 初始化定时任务表
	if err := db.AutoMigrate(&CronTask{}).Error; err != nil {
		log.Printf("初始化定时任务表失败: %v", err)
	}

	// 初始化定时任务组表
	if err := db.AutoMigrate(&CronTaskGroup{}).Error; err != nil {
		log.Printf("初始化定时任务组表失败: %v", err)
	}

	// 初始化定时任务日志表
	if err := db.AutoMigrate(&CronTaskLog{}).Error; err != nil {
		log.Printf("初始化定时任务日志表失败: %v", err)
	}

	// 初始化tmux任务表
	if err := db.AutoMigrate(&TmuxTask{}).Error; err != nil {
		log.Printf("初始化tmux任务表失败: %v", err)
	}

	fmt.Println("定时任务相关表初始化完成")

	// 初始化默认任务组
	initDefaultTaskGroups()
}
