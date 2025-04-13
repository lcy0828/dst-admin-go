package logparser

import (
	"path/filepath"
	"sync"
)

// 全局位置管理器
var globalPositionManager *PositionManager
var positionManagerOnce sync.Once

// GetGlobalPositionManager 获取全局位置管理器
func GetGlobalPositionManager(dstSavePath string) *PositionManager {
	positionManagerOnce.Do(func() {
		// 确定位置记录文件路径
		positionsFile := filepath.Join(dstSavePath, "log_positions.json")

		// 创建位置管理器
		manager, err := NewPositionManager(positionsFile)
		if err != nil {
			// 如果创建失败，使用空的位置管理器
			manager = &PositionManager{
				positions:     make(map[string]*LogPosition),
				positionsFile: positionsFile,
				saveInterval:  defaultSaveInterval,
				stopChan:      make(chan struct{}),
			}
		}

		globalPositionManager = manager
		globalPositionManager.Start()
	})

	return globalPositionManager
}

// ShutdownGlobalPositionManager 关闭全局位置管理器
func ShutdownGlobalPositionManager() {
	if globalPositionManager != nil {
		globalPositionManager.Stop()
	}
}

// ShutdownAllLogParsers 关闭所有日志解析器
func ShutdownAllLogParsers() {
	// 获取日志解析器管理器
	manager := GetLogParserManager()

	// 关闭所有解析器
	manager.ShutdownAllParsers()
}
