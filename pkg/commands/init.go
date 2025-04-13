package commands

import (
	"dont/models"
	"dont/pkg/commands/builtin"
	"log"
)

// InitBuiltinCommands 手动初始化内置命令
// 这个函数可以在需要时手动调用，强制初始化内置命令
func InitBuiltinCommands() error {
	log.Println("[CommandManager] 开始手动初始化内置命令...")

	// 获取内置命令
	builtinCommands := builtin.GetBuiltinCommands()
	log.Printf("[CommandManager] 获取到 %d 个内置命令定义", len(builtinCommands))

	// 将内置命令保存到数据库
	if err := models.InitBuiltinCommands(builtinCommands); err != nil {
		log.Printf("[CommandManager][ERROR] 手动初始化内置命令失败: %v", err)
		return err
	}

	log.Println("[CommandManager] 手动初始化内置命令成功")
	return nil
}
