package cron

import (
	"dont/models"
	"dont/pkg/commands"
	"dont/tmux"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// 玩家列表命令ID
const PlayerListCommandID = "list_players"

// 玩家列表日志类型
const PlayerListLogType = "listplayers"

// UpdatePlayerInfo 更新玩家信息
// 参数:
// - sessionName: tmux会话名称，格式为 dstserver_存档名_世界名
func UpdatePlayerInfo(sessionName string) (string, error) {
	log.Printf("[PlayerTask] 开始更新玩家信息，会话名: %s", sessionName)

	// 解析会话名称获取存档和世界信息
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		return "", fmt.Errorf("会话名称格式不正确: %s，应为 dstserver_存档名_世界名", sessionName)
	}

	archiveName := parts[1]
	worldName := parts[2]

	log.Printf("[PlayerTask] 解析会话名称: 存档=%s, 世界=%s", archiveName, worldName)

	// 获取命令管理器
	cmdManager := commands.CreateCommandManager("./conf/commands.json")
	if err := cmdManager.Initialize(); err != nil {
		return "", fmt.Errorf("初始化命令管理器失败: %v", err)
	}

	// 获取玩家列表命令
	cmd, err := cmdManager.GetCommand(PlayerListCommandID)
	if err != nil || cmd == nil {
		return "", fmt.Errorf("未找到玩家列表命令: %s, 错误: %v", PlayerListCommandID, err)
	}

	// 生成脚本
	script := cmd.GenerateScript()

	// 从其他包导入路径配置
	// 导入 routers/tmux 包中的函数
	savePath := ""
	ugcPath := ""
	serverPath := ""
	serverMode := "64"

	// 使用环境变量或默认值
	if envPath := os.Getenv("DST_SAVE_PATH"); envPath != "" {
		savePath = envPath
	} else {
		savePath = "./Klei/DoNotStarveTogether" // 默认路径
	}

	if envPath := os.Getenv("DST_UGC_PATH"); envPath != "" {
		ugcPath = envPath
	} else {
		ugcPath = "./dstserver/ugc_mods" // 默认路径
	}

	if envPath := os.Getenv("DST_SERVER_PATH"); envPath != "" {
		serverPath = envPath
	} else {
		serverPath = "./dstserver" // 默认路径
	}

	// 获取服务器实例
	server, err := tmux.NewDSTServer(
		archiveName,
		worldName,
		ugcPath,
		savePath,
		"DoNotStarveTogether",
		serverPath,
		serverMode,
	)
	if err != nil {
		return "", fmt.Errorf("创建服务器实例失败: %v", err)
	}

	// 检查服务器是否在运行
	running, err := server.IsRunning()
	if err != nil {
		log.Printf("[PlayerTask] 检查服务器运行状态失败: %v", err)
		return "", fmt.Errorf("检查服务器运行状态失败: %v", err)
	}

	if !running {
		// 服务器未运行，将该存档中的所有在线玩家状态更新为离线
		log.Printf("[PlayerTask] 服务器未运行: %s，将存档 %s 中的所有在线玩家状态更新为离线", sessionName, archiveName)

		if err := models.SetAllPlayersOffline(archiveName); err != nil {
			log.Printf("[PlayerTask] 更新玩家状态失败: %v", err)
			return "", fmt.Errorf("更新玩家状态失败: %v", err)
		}

		// 获取玩家统计信息
		stats, err := models.GetPlayerStats(archiveName)
		if err != nil {
			return "", fmt.Errorf("获取玩家统计信息失败: %v", err)
		}

		message := fmt.Sprintf("服务器未运行，已将存档 %s 中的所有在线玩家状态更新为离线，总玩家数: %d",
			archiveName, stats["total_count"])
		log.Printf("[PlayerTask] %s", message)
		return message, nil
	}

	// 发送命令
	if err := server.SendCommand(script); err != nil {
		return "", fmt.Errorf("发送命令失败: %v", err)
	}

	log.Printf("[PlayerTask] 已发送玩家列表命令: %s", script)

	// 等待一段时间，确保命令执行完成并日志已写入
	time.Sleep(2 * time.Second)

	// 查询最新的玩家列表日志
	log.Printf("[PlayerTask] 开始查询玩家列表日志, 存档: %s, 世界: %s", archiveName, worldName)

	// 设置时间范围为过去5分钟到现在，增加查询范围
	startTime := time.Now().Add(-5 * time.Minute)
	endTime := time.Now()
	log.Printf("[PlayerTask] 查询时间范围: %s 至 %s", startTime.Format("2006-01-02 15:04:05"), endTime.Format("2006-01-02 15:04:05"))

	logs, total, err := models.GetGameLogs(archiveName, worldName, PlayerListLogType, "", startTime, endTime, 1, 10)
	if err != nil {
		log.Printf("[PlayerTask] 获取玩家列表日志失败: %v", err)
		return "", fmt.Errorf("获取玩家列表日志失败: %v", err)
	}

	log.Printf("[PlayerTask] 查询到 %d 条玩家列表日志, 总计: %d", len(logs), total)

	if len(logs) == 0 {
		log.Printf("[PlayerTask] 未找到最新的玩家列表日志")
		return "未找到最新的玩家列表日志", nil
	}

	// 获取最新的日志
	latestLog := logs[0]
	log.Printf("[PlayerTask] 最新日志ID: %d, 时间戳: %s", latestLog.ID, latestLog.Timestamp.Format("2006-01-02 15:04:05"))
	log.Printf("[PlayerTask] 日志内容: %s", latestLog.RawContent)

	// 检查 models.UpdatePlayersFromLog 函数是否存在
	log.Printf("[PlayerTask] 开始更新玩家信息, 存档: %s", archiveName)

	// 更新玩家信息
	if err := models.UpdatePlayersFromLog(archiveName, latestLog.RawContent); err != nil {
		log.Printf("[PlayerTask] 更新玩家信息失败: %v", err)
		return "", fmt.Errorf("更新玩家信息失败: %v", err)
	}

	log.Printf("[PlayerTask] 玩家信息更新成功, 存档: %s", archiveName)

	// 获取玩家统计信息
	stats, err := models.GetPlayerStats(archiveName)
	if err != nil {
		return "", fmt.Errorf("获取玩家统计信息失败: %v", err)
	}

	// 构建返回消息
	message := fmt.Sprintf("玩家信息更新成功，存档: %s，在线玩家: %d，总玩家: %d",
		archiveName,
		stats["online_count"],
		stats["total_count"])

	log.Printf("[PlayerTask] %s", message)

	return message, nil
}

// RegisterPlayerTasks 注册玩家相关任务
func RegisterPlayerTasks(manager *TaskManager) {
	// 注册更新玩家信息任务
	manager.RegisterFunction("update_player_info", UpdatePlayerInfo)

	// 创建默认的玩家信息更新任务
	CreateDefaultPlayerInfoTask()

	log.Println("[PlayerTask] 玩家相关任务注册完成")
}

// CreateDefaultPlayerInfoTask 创建默认的玩家信息更新任务
func CreateDefaultPlayerInfoTask() {
	// 检查是否已存在玩家信息更新任务
	tasks, err := models.GetAllTasks()
	if err != nil {
		log.Printf("[PlayerTask] 获取任务列表失败: %v", err)
		return
	}

	// 检查是否已存在玩家信息更新任务
	hasPlayerInfoTask := false
	for _, task := range tasks {
		if task.Type == "function" && task.Target == "update_player_info" {
			hasPlayerInfoTask = true
			break
		}
	}

	// 如果已存在玩家信息更新任务，则不创建
	if hasPlayerInfoTask {
		log.Println("[PlayerTask] 已存在玩家信息更新任务，不创建默认任务")
		return
	}

	// 获取任务组
	groups, err := models.GetAllTaskGroups()
	if err != nil || len(groups) == 0 {
		log.Printf("[PlayerTask] 获取任务组失败: %v", err)
		return
	}

	// 使用第一个任务组作为默认组
	groupID := groups[0].ID

	// 获取运行中的服务器列表
	// 直接使用默认服务器信息，因为我们无法直接调用 tmux.GetRunningServers
	// 在实际应用中，可以通过其他方式获取服务器列表
	// 这里我们使用一个默认的服务器信息作为示例
	type ServerInfo struct {
		ArchiveName string
		WorldName   string
		Status      string
	}

	// 创建一个默认的服务器列表
	servers := []ServerInfo{
		{
			ArchiveName: "MyCluster",
			WorldName:   "Master",
			Status:      "running",
		},
	}

	if len(servers) == 0 {
		log.Println("[PlayerTask] 没有运行中的服务器，不创建默认任务")
		return
	}

	// 为每个运行中的服务器创建任务
	for _, server := range servers {
		// 构建会话名称
		sessionName := fmt.Sprintf("dstserver_%s_%s", server.ArchiveName, server.WorldName)

		// 创建任务
		task := &models.CronTask{
			Name:          fmt.Sprintf("更新玩家信息 - %s", server.ArchiveName),
			Description:   fmt.Sprintf("每分钟更新一次 %s 存档的玩家信息", server.ArchiveName),
			GroupID:       groupID,
			Spec:          "0 */1 * * * *", // 每分钟执行一次
			Type:          "function",
			Target:        "update_player_info",
			Timeout:       30, // 30秒超时
			RetryTimes:    3,  // 重试3次
			RetryInterval: 10, // 重试间隔10秒
			Status:        1,  // 启用
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}

		// 设置参数
		if err := task.SetArgs([]interface{}{sessionName}); err != nil {
			log.Printf("[PlayerTask] 设置任务参数失败: %v", err)
			continue
		}

		// 添加任务
		if err := models.AddTask(task); err != nil {
			log.Printf("[PlayerTask] 添加任务失败: %v", err)
			continue
		}

		log.Printf("[PlayerTask] 成功创建玩家信息更新任务: %s", task.Name)
	}
}
