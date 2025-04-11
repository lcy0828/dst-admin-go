package tmux

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
	serverInfoMapMutex.Lock()
	defer serverInfoMapMutex.Unlock()

	var result []ServerInfo
	for _, info := range serverInfoMap {
		if info.Status == "running" {
			// 创建一个副本
			serverInfo := ServerInfo{
				SessionName:    info.SessionName,
				ArchiveName:    info.ArchiveName,
				WorldName:      info.WorldName,
				ServerMode:     info.ServerMode,
				StartDirectory: info.StartDirectory,
				Status:         info.Status,
				StartTime:      info.StartTime,
			}
			result = append(result, serverInfo)
		}
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
