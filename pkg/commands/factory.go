package commands

// CreateCommandManager 创建命令管理器
// 这是一个工厂函数，返回CommandManagerInterface接口
func CreateCommandManager(storagePath string) CommandManagerInterface {
	manager := NewCommandManager()
	return manager
}
