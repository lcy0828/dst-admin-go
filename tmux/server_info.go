package tmux

import (
	"log"
	"time"
)

// GetServerInfoMap 获取服务器信息映射的副本
// 这个函数可以被其他包直接调用，避免通过HTTP请求获取服务器状态
func GetServerInfoMap() map[string]*ServerInfo {
	serverInfoMapMutex.Lock()
	defer serverInfoMapMutex.Unlock()

	// 创建一个副本
	result := make(map[string]*ServerInfo)
	for k, v := range serverInfoMap {
		// 创建一个新的ServerInfo对象，避免引用原始对象
		info := &ServerInfo{
			SessionName:    v.SessionName,
			ArchiveName:    v.ArchiveName,
			WorldName:      v.WorldName,
			ServerMode:     v.ServerMode,
			StartDirectory: v.StartDirectory,
			Status:         v.Status,
			StartTime:      v.StartTime,
		}
		result[k] = info
	}

	return result
}

// GetRunningServers 获取所有运行中的服务器信息
// 这个函数可以被其他包直接调用，避免通过HTTP请求获取服务器状态
func GetRunningServers() []ServerInfo {
	// 直接调用ListDSTServers函数获取服务器列表
	servers, err := ListDSTServers()
	if err != nil {
		log.Printf("[TMUX] 获取服务器列表失败: %v", err)
		return []ServerInfo{}
	}

	// 筛选出运行中的服务器
	var result []ServerInfo
	for _, server := range servers {
		if server.Status == "running" {
			result = append(result, server)
		}
	}

	log.Printf("[TMUX] 获取到 %d 个运行中的服务器", len(result))
	for i, server := range result {
		log.Printf("[TMUX] 运行中的服务器 #%d: 会话=%s, 存档=%s, 世界=%s",
			i+1, server.SessionName, server.ArchiveName, server.WorldName)
	}

	return result
}

// UpdateServerStatus 更新服务器状态
func UpdateServerStatus(sessionName, status string) {
	serverInfoMapMutex.Lock()
	defer serverInfoMapMutex.Unlock()

	if info, exists := serverInfoMap[sessionName]; exists {
		info.Status = status
	}
}

// AddTestServer 添加测试服务器数据
func AddTestServer() {
	serverInfoMapMutex.Lock()
	defer serverInfoMapMutex.Unlock()

	// 添加测试数据
	serverInfoMap["dstserver_test_forest"] = &ServerInfo{
		SessionName:    "dstserver_test_forest",
		ArchiveName:    "test",
		WorldName:      "Forest",
		ServerMode:     "64bit",
		StartDirectory: "/root/DST",
		Status:         "running",
		StartTime:      time.Now().Format(time.RFC3339),
	}

	log.Printf("[TMUX] 添加测试服务器数据: dstserver_test_forest")
}
