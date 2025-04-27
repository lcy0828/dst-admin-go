package cron

import (
	"dont/models"
	"dont/tmux"
	"fmt"
	"log"
	"strings"
	"time"
)

// MonitorServerStatus 监控服务器状态并管理玩家信息和世界状态定时任务
// 每30秒检测一次服务器是否正在运行
// 如果有服务器运行，确保玩家信息和世界状态定时任务启用
// 如果没有服务器运行，禁用这些定时任务
func MonitorServerStatus() (string, error) {
	log.Printf("[ServerMonitor] 开始监控服务器状态")

	// 获取所有运行中的服务器
	runningServers := tmux.GetRunningServers(true)
	log.Printf("[ServerMonitor] 检测到 %d 个运行中的服务器", len(runningServers))

	// 获取所有定时任务
	tasks, err := models.GetAllTasks()
	if err != nil {
		log.Printf("[ServerMonitor] 获取任务列表失败: %v", err)
		return "", fmt.Errorf("获取任务列表失败: %v", err)
	}

	// 筛选出与玩家配置相关的任务
	playerConfigTasks := make(map[string]*models.CronTask)
	// 筛选出与世界状态相关的任务
	worldStateTasks := make(map[string]*models.CronTask)

	for i := range tasks {
		task := &tasks[i]
		// 获取任务参数（存档名和世界名）
		args, err := task.GetArgs()
		if err != nil || len(args) < 2 {
			continue
		}

		// 构建会话名称
		archiveName, ok1 := args[0].(string)
		worldName, ok2 := args[1].(string)
		if !ok1 || !ok2 {
			continue
		}

		sessionName := fmt.Sprintf("dstserver_%s_%s", archiveName, worldName)

		// 根据任务类型分类
		if task.Type == "function" {
			if task.Target == "read_player_config" {
				playerConfigTasks[sessionName] = task
			} else if task.Target == "read_world_state" {
				worldStateTasks[sessionName] = task
			}
		}
	}

	log.Printf("[ServerMonitor] 找到 %d 个玩家配置任务, %d 个世界状态任务",
		len(playerConfigTasks), len(worldStateTasks))

	// 如果没有运行中的服务器，禁用所有相关任务
	if len(runningServers) == 0 {
		log.Printf("[ServerMonitor] 没有运行中的服务器，禁用所有相关任务")

		// 禁用玩家配置任务
		for _, task := range playerConfigTasks {
			if task.Status == 1 { // 如果任务当前是启用状态
				if err := models.UpdateTaskStatus(task.ID, 0); err != nil {
					log.Printf("[ServerMonitor] 禁用玩家配置任务失败 (ID: %d): %v", task.ID, err)
				} else {
					log.Printf("[ServerMonitor] 已禁用玩家配置任务: %s (ID: %d)", task.Name, task.ID)
				}
			}
		}

		// 禁用世界状态任务
		for _, task := range worldStateTasks {
			if task.Status == 1 { // 如果任务当前是启用状态
				if err := models.UpdateTaskStatus(task.ID, 0); err != nil {
					log.Printf("[ServerMonitor] 禁用世界状态任务失败 (ID: %d): %v", task.ID, err)
				} else {
					log.Printf("[ServerMonitor] 已禁用世界状态任务: %s (ID: %d)", task.Name, task.ID)
				}
			}
		}

		return "没有运行中的服务器，已禁用所有相关任务", nil
	}

	// 如果有运行中的服务器，为每个服务器启用或创建相应的任务
	taskManager := GetTaskManager()

	// 按存档名分组服务器，以便于选择主世界
	archiveServers := make(map[string][]tmux.ServerInfo)
	for _, server := range runningServers {
		archiveServers[server.ArchiveName] = append(archiveServers[server.ArchiveName], server)
	}

	// 处理每个存档
	for _, servers := range archiveServers {
		// 首先尝试找到主世界
		var masterServer *tmux.ServerInfo
		var forestServer *tmux.ServerInfo

		// 首先尝试找到 is_master 为 true 的世界
		for i, server := range servers {
			if server.IsMaster {
				masterServer = &servers[i]
				log.Printf("[ServerMonitor] 找到主世界: %s (存档: %s, 世界: %s)",
					server.SessionName, server.ArchiveName, server.WorldName)
				break
			}

			// 同时记录包含 forest 的世界作为备用
			if strings.Contains(strings.ToLower(server.WorldName), "forest") && forestServer == nil {
				forestServer = &servers[i]
			}
		}

		// 如果没有找到 is_master 为 true 的世界，则使用包含 forest 的世界
		if masterServer == nil && forestServer != nil {
			masterServer = forestServer
			log.Printf("[ServerMonitor] 未找到主世界，使用包含 forest 的世界: %s (存档: %s, 世界: %s)",
				masterServer.SessionName, masterServer.ArchiveName, masterServer.WorldName)
		}

		// 如果仍然没有找到适合的世界，则使用第一个世界
		if masterServer == nil && len(servers) > 0 {
			masterServer = &servers[0]
			log.Printf("[ServerMonitor] 未找到主世界或包含 forest 的世界，使用第一个世界: %s (存档: %s, 世界: %s)",
				masterServer.SessionName, masterServer.ArchiveName, masterServer.WorldName)
		}

		// 如果找到了适合的世界，则为其创建或启用任务
		if masterServer != nil {
			sessionName := masterServer.SessionName
			archiveName := masterServer.ArchiveName
			worldName := masterServer.WorldName

			// 1. 处理玩家配置任务
			// 检查是否已存在相应的任务
			if task, exists := playerConfigTasks[sessionName]; exists {
				// 如果任务存在但被禁用，则启用它
				if task.Status == 0 {
					if err := models.UpdateTaskStatus(task.ID, 1); err != nil {
						log.Printf("[ServerMonitor] 启用玩家配置任务失败 (ID: %d): %v", task.ID, err)
					} else {
						log.Printf("[ServerMonitor] 已启用玩家配置任务: %s (ID: %d)", task.Name, task.ID)
					}
				}
			} else {
				// 如果任务不存在，创建新任务
				taskName := fmt.Sprintf("read_player_config_%s_%s", archiveName, worldName)
				newTask := &models.CronTask{
					Name:        taskName,
					Description: fmt.Sprintf("读取存档 %s 世界 %s 的玩家配置文件", archiveName, worldName),
					Spec:        "*/5 * * * * *", // 每5秒执行一次
					Type:        "function",
					Target:      "read_player_config",
					Status:      1, // 启用
					CreatedAt:   time.Now(),
					UpdatedAt:   time.Now(),
				}

				// 设置参数
				if err := newTask.SetArgs([]interface{}{archiveName, worldName}); err != nil {
					log.Printf("[ServerMonitor] 设置玩家配置任务参数失败: %v", err)
					continue
				}

				// 添加任务
				if err := taskManager.AddTask(newTask); err != nil {
					log.Printf("[ServerMonitor] 创建玩家配置任务失败: %v", err)
				} else {
					log.Printf("[ServerMonitor] 创建玩家配置任务成功: %s", taskName)
				}
			}

			// 2. 处理世界状态任务
			// 检查是否已存在相应的任务
			if task, exists := worldStateTasks[sessionName]; exists {
				// 如果任务存在但被禁用，则启用它
				if task.Status == 0 {
					if err := models.UpdateTaskStatus(task.ID, 1); err != nil {
						log.Printf("[ServerMonitor] 启用世界状态任务失败 (ID: %d): %v", task.ID, err)
					} else {
						log.Printf("[ServerMonitor] 已启用世界状态任务: %s (ID: %d)", task.Name, task.ID)
					}
				}
			} else {
				// 如果任务不存在，创建新任务
				taskName := fmt.Sprintf("read_world_state_%s_%s", archiveName, worldName)
				newTask := &models.CronTask{
					Name:        taskName,
					Description: fmt.Sprintf("读取存档 %s 世界 %s 的世界状态文件", archiveName, worldName),
					Spec:        "*/5 * * * * *", // 每5秒执行一次
					Type:        "function",
					Target:      "read_world_state",
					Status:      1, // 启用
					CreatedAt:   time.Now(),
					UpdatedAt:   time.Now(),
				}

				// 设置参数
				if err := newTask.SetArgs([]interface{}{archiveName, worldName}); err != nil {
					log.Printf("[ServerMonitor] 设置世界状态任务参数失败: %v", err)
					continue
				}

				// 添加任务
				if err := taskManager.AddTask(newTask); err != nil {
					log.Printf("[ServerMonitor] 创建世界状态任务失败: %v", err)
				} else {
					log.Printf("[ServerMonitor] 创建世界状态任务成功: %s", taskName)
				}
			}
		}
	}

	// 禁用不在运行中的服务器对应的任务
	runningSessionMap := make(map[string]bool)
	for _, server := range runningServers {
		runningSessionMap[server.SessionName] = true
	}

	// 禁用不在运行中的服务器对应的玩家配置任务
	for sessionName, task := range playerConfigTasks {
		if !runningSessionMap[sessionName] && task.Status == 1 {
			if err := models.UpdateTaskStatus(task.ID, 0); err != nil {
				log.Printf("[ServerMonitor] 禁用玩家配置任务失败 (ID: %d): %v", task.ID, err)
			} else {
				log.Printf("[ServerMonitor] 已禁用玩家配置任务: %s (ID: %d)", task.Name, task.ID)
			}
		}
	}

	// 禁用不在运行中的服务器对应的世界状态任务
	for sessionName, task := range worldStateTasks {
		if !runningSessionMap[sessionName] && task.Status == 1 {
			if err := models.UpdateTaskStatus(task.ID, 0); err != nil {
				log.Printf("[ServerMonitor] 禁用世界状态任务失败 (ID: %d): %v", task.ID, err)
			} else {
				log.Printf("[ServerMonitor] 已禁用世界状态任务: %s (ID: %d)", task.Name, task.ID)
			}
		}
	}

	return fmt.Sprintf("已检测到 %d 个运行中的服务器，并相应地管理了玩家配置和世界状态任务", len(runningServers)), nil
}

// RegisterServerMonitorTasks 注册服务器监控相关任务
func RegisterServerMonitorTasks(manager *TaskManager) {
	// 注册服务器状态监控函数
	manager.RegisterFunctionWithInfo("monitor_server_status", MonitorServerStatus, FunctionInfo{
		Name:        "monitor_server_status",
		Description: "监控服务器状态并管理玩家信息和世界状态定时任务",
		ParamTypes:  []string{}, // 无参数
	})

	// 创建服务器监控任务
	tasks, err := models.GetAllTasks()
	taskExists := false
	if err == nil {
		for _, t := range tasks {
			if t.Target == "monitor_server_status" {
				taskExists = true
				break
			}
		}
	}

	if !taskExists {
		// 创建任务
		task := &models.CronTask{
			Name:        "服务器状态监控",
			Description: "监控服务器状态并管理玩家信息和世界状态定时任务",
			Spec:        "*/30 * * * * *", // 每30秒执行一次
			Type:        "function",
			Target:      "monitor_server_status",
			Status:      1, // 启用
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}

		// 添加任务
		if err := manager.AddTask(task); err != nil {
			log.Printf("[ServerMonitor] 创建服务器监控任务失败: %v", err)
		} else {
			log.Printf("[ServerMonitor] 创建服务器监控任务成功")
		}
	} else {
		log.Printf("[ServerMonitor] 服务器监控任务已存在，跳过创建")
	}

	log.Println("[ServerMonitor] 服务器监控相关任务注册完成")
}
