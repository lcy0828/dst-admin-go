package models

import (
	"fmt"
	"log"
	"time"
)

// CallCronFunction 调用 cron 包中的函数
// 参数:
// - functionName: 函数名称
// - args: 函数参数
// 返回:
// - string: 函数执行结果
// - error: 错误信息
func CallCronFunction(functionName string, args ...interface{}) (string, error) {
	// 构建命令字符串
	command := functionName + "("
	for i, arg := range args {
		if i > 0 {
			command += ", "
		}
		// 处理不同类型的参数
		switch v := arg.(type) {
		case string:
			command += fmt.Sprintf("\"%s\"", v)
		case int, int32, int64, float32, float64, bool:
			command += fmt.Sprintf("%v", v)
		default:
			command += fmt.Sprintf("%v", v)
		}
	}
	command += ")"

	log.Printf("[CallCronFunction] 执行命令: %s", command)

	// 创建一个临时任务
	task := &CronTask{
		Name:        "temp_" + functionName,
		Description: "临时任务",
		Type:        "function",
		Command:     command,
	}

	// 直接执行命令
	// 注意：这里我们不能直接调用 cron.GetTaskManager().ExecuteTaskCommand
	// 因为这会导致循环导入。直接调用 API 接口来执行命令。

	// 构建一个临时任务并添加到数据库
	task.Status = 1  // 启用
	task.GroupID = 1 // 默认组
	task.CreatedAt = time.Now()
	task.UpdatedAt = time.Now()

	// 添加任务到数据库
	if err := AddTask(task); err != nil {
		log.Printf("[CallCronFunction] 添加任务失败: %v", err)
		return "", err
	}

	// 执行任务
	if err := RunTask(task.ID); err != nil {
		log.Printf("[CallCronFunction] 执行任务失败: %v", err)
		return "", err
	}

	// 等待任务执行完成
	time.Sleep(1 * time.Second)

	// 获取任务日志
	logs, err := GetTaskLogsByTaskID(task.ID, 1, 0)
	if err != nil || len(logs) == 0 {
		log.Printf("[CallCronFunction] 获取任务日志失败: %v", err)
		return "", fmt.Errorf("执行任务失败")
	}

	// 删除任务
	if err := DeleteTask(task.ID); err != nil {
		log.Printf("[CallCronFunction] 删除任务失败: %v", err)
		// 不返回错误，继续处理
	}

	// 返回任务输出
	result := logs[0].Output
	if logs[0].Status == 0 {
		return result, fmt.Errorf(logs[0].Error)
	}

	log.Printf("[CallCronFunction] 执行命令成功: %s", result)
	return result, nil
}
