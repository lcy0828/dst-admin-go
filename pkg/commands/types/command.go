package types

import "strings"

// Command 表示一个可以发送到游戏服务器的命令
type Command struct {
	// 命令的唯一标识符
	ID string `json:"id"`
	// 命令的名称
	Name string `json:"name"`
	// 命令的描述
	Description string `json:"description"`
	// 命令的类别
	Category string `json:"category"`
	// 命令是否为内置命令
	IsBuiltin bool `json:"is_builtin"`
	// 命令的实际执行脚本
	Script string `json:"script"`
	// 是否需要参数
	NeedsParams bool `json:"needs_params"`
	// 参数描述，如果需要参数
	ParamDesc string `json:"param_desc,omitempty"`
	// 命令的使用示例
	Example string `json:"example,omitempty"`
}

// GenerateScript 根据提供的参数生成完整的脚本
// 对于不需要参数的命令，直接返回原始脚本
// 对于需要参数的命令，会替换参数占位符
func (c *Command) GenerateScript(params ...string) string {
	if !c.NeedsParams || len(params) == 0 {
		return c.Script
	}

	// 这里实现参数替换逻辑
	// 目前假设脚本中使用 {1}, {2} 等占位符表示参数位置
	result := c.Script
	for i, param := range params {
		placeholder := "{" + string(rune('1'+i)) + "}"
		// 简单字符串替换
		result = strings.Replace(result, placeholder, param, -1)
	}
	return result
}

// ValidateParams 验证参数是否有效
func (c *Command) ValidateParams(params ...string) bool {
	// 如果不需要参数，但提供了参数，返回false
	if !c.NeedsParams && len(params) > 0 {
		return false
	}

	// 如果需要参数但没有提供，返回false
	if c.NeedsParams && len(params) == 0 {
		return false
	}

	// 实际应用中可以添加更复杂的参数验证逻辑
	return true
}
