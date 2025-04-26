package cron

import (
	"log"
)

// 玩家列表命令ID
const PlayerListCommandID = "list_players"

// 玩家列表日志类型
const PlayerListLogType = "listplayers"

// RegisterPlayerTasks 注册玩家相关任务
func RegisterPlayerTasks(manager *TaskManager) {
	// 注册更新玩家信息任务已移除

	log.Println("[PlayerTask] 玩家相关任务注册完成")
}

// CreateDefaultPlayerInfoTask 创建默认的玩家信息更新任务
// 注意: 此函数已经被移除，保留函数签名以避免其他地方的调用出错
func CreateDefaultPlayerInfoTask() {
	// 函数已经被移除，不再创建默认的玩家信息更新任务
	log.Println("[PlayerTask] CreateDefaultPlayerInfoTask 函数已经被移除，不再创建默认的玩家信息更新任务")
}
