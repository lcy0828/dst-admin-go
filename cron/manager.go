package cron

import (
	"bytes"
	"context"
	"dont/models"
	"dont/service/logparser"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// FunctionInfo 函数信息
type FunctionInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ParamTypes  []string `json:"param_types"`
}

// TaskManager 定时任务管理器
type TaskManager struct {
	cron            *cron.Cron
	entryMap        map[int]cron.EntryID    // 任务ID到cron EntryID的映射
	functionMap     map[string]interface{}  // 函数名到函数的映射
	functionInfoMap map[string]FunctionInfo // 函数信息映射
	mutex           sync.RWMutex
	isRunning       bool
}

var (
	manager     *TaskManager
	managerOnce sync.Once
)

// GetTaskManager 获取任务管理器单例
func GetTaskManager() *TaskManager {
	managerOnce.Do(func() {
		manager = &TaskManager{
			cron:            cron.New(cron.WithSeconds()), // 使用秒级精度
			entryMap:        make(map[int]cron.EntryID),
			functionMap:     make(map[string]interface{}),
			functionInfoMap: make(map[string]FunctionInfo),
		}
		// 注册内置函数
		manager.registerBuiltinFunctions()
	})
	return manager
}

// 注册内置函数
func (m *TaskManager) registerBuiltinFunctions() {
	// 获取日志解析器管理器
	logManager := logparser.GetLogParserManager()

	// 注册日志清理函数
	m.RegisterFunctionWithInfo("cleanupLogs", func(retentionDays int) {
		// 计算保留日期
		retentionDate := time.Now().AddDate(0, 0, -retentionDays)

		// 清理旧日志
		if err := models.ClearOldLogs(retentionDate); err != nil {
			log.Printf("[CronTask] 清理旧日志失败: %v", err)
		} else {
			log.Printf("[CronTask] 已清理 %s 之前的日志", retentionDate.Format("2006-01-02"))
		}

		// 清理旧任务日志
		if err := models.ClearOldTaskLogs(retentionDate); err != nil {
			log.Printf("[CronTask] 清理旧任务日志失败: %v", err)
		} else {
			log.Printf("[CronTask] 已清理 %s 之前的任务日志", retentionDate.Format("2006-01-02"))
		}
	}, FunctionInfo{
		Name:        "cleanupLogs",
		Description: "清理旧日志",
		ParamTypes:  []string{"int"}, // 保留天数
	})

	// 注册数据库备份函数
	m.RegisterFunctionWithInfo("backupDatabase", func(backupPath string) {
		log.Printf("[CronTask] 开始备份数据库到 %s", backupPath)
		// 这里实现数据库备份逻辑
		// 获取数据库文件路径
		dbPath := "./go-dont.db" // 默认路径

		// 创建备份目录
		if err := os.MkdirAll(backupPath, 0755); err != nil {
			log.Printf("[CronTask] 创建备份目录失败: %v", err)
			return
		}

		// 生成备份文件名
		backupFile := filepath.Join(backupPath, fmt.Sprintf("db_backup_%s.db", time.Now().Format("20060102_150405")))

		// 复制数据库文件
		cmd := exec.Command("cp", dbPath, backupFile)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("[CronTask] 备份数据库失败: %v, 输出: %s", err, string(output))
			return
		}

		log.Printf("[CronTask] 数据库备份完成: %s", backupFile)
	}, FunctionInfo{
		Name:        "backupDatabase",
		Description: "备份数据库",
		ParamTypes:  []string{"string"}, // 备份路径
	})

	// 注册系统状态检查函数
	m.RegisterFunctionWithInfo("checkSystemStatus", func() {
		log.Printf("[CronTask] 开始检查系统状态")
		// 这里实现系统状态检查逻辑
		// 检查磁盘空间
		cmd := exec.Command("df", "-h")
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("[CronTask] 检查磁盘空间失败: %v", err)
		} else {
			log.Printf("[CronTask] 磁盘空间信息:\n%s", string(output))
		}

		// 检查内存使用
		cmd = exec.Command("free", "-h")
		output, err = cmd.CombinedOutput()
		if err != nil {
			log.Printf("[CronTask] 检查内存使用失败: %v", err)
		} else {
			log.Printf("[CronTask] 内存使用信息:\n%s", string(output))
		}

		log.Printf("[CronTask] 系统状态检查完成")
	}, FunctionInfo{
		Name:        "checkSystemStatus",
		Description: "检查系统状态",
		ParamTypes:  []string{}, // 无参数
	})

	// 注册日志解析器重载函数
	m.RegisterFunctionWithInfo("reloadLogParsers", func() {
		log.Printf("[CronTask] 开始重载日志解析器")
		if err := logManager.ReloadAllParsers(); err != nil {
			log.Printf("[CronTask] 重载日志解析器失败: %v", err)
		} else {
			log.Printf("[CronTask] 日志解析器重载成功")
		}
	}, FunctionInfo{
		Name:        "reloadLogParsers",
		Description: "重载日志解析器",
		ParamTypes:  []string{}, // 无参数
	})

	// 可以根据需要添加更多内置函数
}

// RegisterFunction 注册自定义函数
func (m *TaskManager) RegisterFunction(name string, fn interface{}) {
	m.RegisterFunctionWithInfo(name, fn, FunctionInfo{
		Name:        name,
		Description: "",
		ParamTypes:  []string{},
	})
}

// RegisterFunctionWithInfo 注册带有信息的自定义函数
func (m *TaskManager) RegisterFunctionWithInfo(name string, fn interface{}, info FunctionInfo) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// 检查函数类型
	if reflect.TypeOf(fn).Kind() != reflect.Func {
		log.Printf("[CronTask] 注册函数失败: %s 不是一个函数", name)
		return
	}

	m.functionMap[name] = fn
	m.functionInfoMap[name] = info
	log.Printf("[CronTask] 成功注册函数: %s", name)
}

// Start 启动任务管理器
func (m *TaskManager) Start() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.isRunning {
		return fmt.Errorf("任务管理器已经在运行中")
	}

	// 检查是否需要初始化默认任务
	m.initDefaultTasksIfNeeded()

	// 加载所有启用的任务
	tasks, err := models.GetEnabledTasks()
	if err != nil {
		return fmt.Errorf("加载任务失败: %v", err)
	}

	// 添加所有任务到cron
	for _, task := range tasks {
		if err := m.addTaskToCron(&task); err != nil {
			log.Printf("[CronTask] 添加任务失败: %v", err)
			continue
		}
	}

	// 启动cron
	m.cron.Start()
	m.isRunning = true
	log.Printf("[CronTask] 任务管理器已启动，共加载 %d 个任务", len(tasks))
	return nil
}

// initDefaultTasksIfNeeded 如果需要，初始化默认任务
func (m *TaskManager) initDefaultTasksIfNeeded() {
	// 检查是否存在任务
	tasks, err := models.GetAllTasks()
	if err != nil || len(tasks) == 0 {
		// 创建默认的日志清理任务
		defaultTask := &models.CronTask{
			Name:        "每日日志清理",
			Description: "每天凌晨3点清理超过30天的日志",
			Spec:        "0 0 3 * * *", // 每天凌晨3点执行
			Type:        "function",
			Target:      "cleanupLogs",
			Status:      1, // 启用
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}

		// 设置参数
		if err := defaultTask.SetArgs([]interface{}{30}); err != nil {
			log.Printf("[CronTask] 设置默认任务参数失败: %v", err)
		}

		// 添加到数据库
		if err := models.AddTask(defaultTask); err != nil {
			log.Printf("[CronTask] 添加默认任务失败: %v", err)
		} else {
			log.Printf("[CronTask] 添加默认任务成功: %s", defaultTask.Name)
		}
	}
}

// Stop 停止任务管理器
func (m *TaskManager) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.isRunning {
		return
	}

	// 停止cron
	ctx := m.cron.Stop()
	<-ctx.Done() // 等待所有任务完成

	// 清空entryMap
	m.entryMap = make(map[int]cron.EntryID)
	m.isRunning = false
	log.Printf("[CronTask] 任务管理器已停止")
}

// Restart 重启任务管理器
func (m *TaskManager) Restart() error {
	m.Stop()
	return m.Start()
}

// AddTask 添加任务
func (m *TaskManager) AddTask(task *models.CronTask) error {
	// 保存到数据库
	if err := models.AddTask(task); err != nil {
		return err
	}

	// 如果任务管理器正在运行且任务是启用状态，则添加到cron
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.isRunning && task.Status == 1 {
		return m.addTaskToCron(task)
	}

	return nil
}

// UpdateTask 更新任务
func (m *TaskManager) UpdateTask(task *models.CronTask) error {
	// 保存到数据库
	if err := models.UpdateTask(task); err != nil {
		return err
	}

	// 如果任务管理器正在运行，则更新cron中的任务
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.isRunning {
		// 先移除旧任务
		if entryID, exists := m.entryMap[task.ID]; exists {
			m.cron.Remove(entryID)
			delete(m.entryMap, task.ID)
		}

		// 如果任务是启用状态，则添加到cron
		if task.Status == 1 {
			return m.addTaskToCron(task)
		}
	}

	return nil
}

// DeleteTask 删除任务
func (m *TaskManager) DeleteTask(id int) error {
	// 从数据库删除
	if err := models.DeleteTask(id); err != nil {
		return err
	}

	// 如果任务管理器正在运行，则从cron中移除任务
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.isRunning {
		if entryID, exists := m.entryMap[id]; exists {
			m.cron.Remove(entryID)
			delete(m.entryMap, id)
		}
	}

	return nil
}

// EnableTask 启用任务
func (m *TaskManager) EnableTask(id int) error {
	// 更新数据库状态
	if err := models.UpdateTaskStatus(id, 1); err != nil {
		return err
	}

	// 如果任务管理器正在运行，则添加任务到cron
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.isRunning {
		// 先检查任务是否已经在cron中
		if _, exists := m.entryMap[id]; exists {
			return nil
		}

		// 获取任务
		task, err := models.GetTaskByID(id)
		if err != nil {
			return err
		}

		// 添加到cron
		return m.addTaskToCron(task)
	}

	return nil
}

// DisableTask 禁用任务
func (m *TaskManager) DisableTask(id int) error {
	// 更新数据库状态
	if err := models.UpdateTaskStatus(id, 0); err != nil {
		return err
	}

	// 如果任务管理器正在运行，则从cron中移除任务
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.isRunning {
		if entryID, exists := m.entryMap[id]; exists {
			m.cron.Remove(entryID)
			delete(m.entryMap, id)
		}
	}

	return nil
}

// RunTask 立即运行任务
func (m *TaskManager) RunTask(id int) error {
	// 获取任务
	task, err := models.GetTaskByID(id)
	if err != nil {
		return err
	}

	// 执行任务
	go m.executeTask(task)
	return nil
}

// addTaskToCron 添加任务到cron
func (m *TaskManager) addTaskToCron(task *models.CronTask) error {
	// 创建任务函数
	taskFunc := func() {
		m.executeTask(task)
	}

	// 添加到cron
	entryID, err := m.cron.AddFunc(task.Spec, taskFunc)
	if err != nil {
		return err
	}

	// 保存entryID
	m.entryMap[task.ID] = entryID
	log.Printf("[CronTask] 任务已添加到调度器: %s (ID: %d)", task.Name, task.ID)
	return nil
}

// executeTask 执行任务
func (m *TaskManager) executeTask(task *models.CronTask) {
	log.Printf("[CronTask] 开始执行任务: %s (ID: %d)", task.Name, task.ID)
	startTime := time.Now()

	// 更新最后运行时间
	if err := models.UpdateLastRunTime(task.ID, startTime); err != nil {
		log.Printf("[CronTask] 更新任务最后运行时间失败: %v", err)
	}

	// 创建任务日志
	taskLog := &models.CronTaskLog{
		TaskID:    task.ID,
		TaskName:  task.Name,
		StartTime: startTime,
		CreatedAt: time.Now(),
	}

	// 检查依赖任务是否完成
	if task.HasDependency() {
		if err := m.checkDependencies(task); err != nil {
			log.Printf("[CronTask] 任务依赖检查失败: %s (ID: %d), 错误: %v", task.Name, task.ID, err)

			// 更新任务日志
			endTime := time.Now()
			duration := endTime.Sub(startTime).Milliseconds()
			taskLog.EndTime = endTime
			taskLog.Duration = duration
			taskLog.Status = 0
			taskLog.Error = err.Error()
			taskLog.Output = "依赖任务未完成"

			// 保存任务日志
			if err := models.AddTaskLog(taskLog); err != nil {
				log.Printf("[CronTask] 保存任务日志失败: %v", err)
			}

			// 更新任务状态
			if err := models.UpdateTaskLastStatus(task.ID, 0); err != nil {
				log.Printf("[CronTask] 更新任务状态失败: %v", err)
			}

			return
		}
	}

	var err error
	var output string

	// 设置超时处理
	if task.Timeout > 0 {
		// 创建一个带超时的上下文
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(task.Timeout)*time.Second)
		defer cancel()

		// 使用通道来接收执行结果
		resultChan := make(chan struct {
			Output string
			Err    error
		})

		// 在单独的goroutine中执行任务
		go func() {
			var o string
			var e error

			switch task.Type {
			case "function":
				o, e = m.executeFunction(task)
			case "shell":
				o, e = m.executeShell(task)
			default:
				e = fmt.Errorf("不支持的任务类型: %s", task.Type)
			}

			resultChan <- struct {
				Output string
				Err    error
			}{o, e}
		}()

		// 等待结果或超时
		select {
		case result := <-resultChan:
			output = result.Output
			err = result.Err
		case <-ctx.Done():
			// 超时
			output = "任务执行超时"
			err = fmt.Errorf("任务执行超时，超过 %d 秒", task.Timeout)
		}
	} else {
		// 正常执行任务
		switch task.Type {
		case "function":
			output, err = m.executeFunction(task)
		case "shell":
			output, err = m.executeShell(task)
		default:
			err = fmt.Errorf("不支持的任务类型: %s", task.Type)
		}
	}

	// 记录执行结果
	endTime := time.Now()
	duration := endTime.Sub(startTime).Milliseconds()

	// 更新任务日志
	taskLog.EndTime = endTime
	taskLog.Duration = duration
	taskLog.Output = output

	// 处理执行结果
	var taskStatus int
	if err != nil {
		log.Printf("[CronTask] 任务执行失败: %s (ID: %d), 错误: %v", task.Name, task.ID, err)
		taskLog.Status = 0
		taskLog.Error = err.Error()
		taskStatus = 0

		// 如果配置了重试，则进行重试
		if task.RetryTimes > 0 {
			log.Printf("[CronTask] 任务将进行重试: %s (ID: %d), 重试次数: %d", task.Name, task.ID, task.RetryTimes)
			go m.retryTask(task, 1)
		}
	} else {
		log.Printf("[CronTask] 任务执行成功: %s (ID: %d), 耗时: %v", task.Name, task.ID, time.Since(startTime))
		taskLog.Status = 1
		taskStatus = 1
	}

	// 保存任务日志
	if err := models.AddTaskLog(taskLog); err != nil {
		log.Printf("[CronTask] 保存任务日志失败: %v", err)
	}

	// 更新任务状态
	if err := models.UpdateTaskLastStatus(task.ID, taskStatus); err != nil {
		log.Printf("[CronTask] 更新任务状态失败: %v", err)
	}
}

// executeFunction 执行函数类型的任务
func (m *TaskManager) executeFunction(task *models.CronTask) (string, error) {
	m.mutex.RLock()
	fn, exists := m.functionMap[task.Target]
	m.mutex.RUnlock()

	if !exists {
		return "", fmt.Errorf("函数不存在: %s", task.Target)
	}

	// 获取参数
	args, err := task.GetArgs()
	if err != nil {
		return "", fmt.Errorf("解析参数失败: %v", err)
	}

	// 使用反射调用函数
	fnValue := reflect.ValueOf(fn)
	fnType := fnValue.Type()

	// 检查参数数量
	if fnType.NumIn() != len(args) {
		return "", fmt.Errorf("参数数量不匹配: 期望 %d, 实际 %d", fnType.NumIn(), len(args))
	}

	// 准备参数
	callArgs := make([]reflect.Value, len(args))
	for i, arg := range args {
		// 转换参数类型
		argValue := reflect.ValueOf(arg)
		paramType := fnType.In(i)

		// 如果参数是JSON数字，可能需要特殊处理
		if argValue.Kind() == reflect.Float64 && paramType.Kind() == reflect.Int {
			argValue = reflect.ValueOf(int(argValue.Float()))
		}

		callArgs[i] = argValue
	}

	// 创建一个缓冲区来捕获输出
	var outputBuffer bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(io.MultiWriter(oldOutput, &outputBuffer))
	defer log.SetOutput(oldOutput)

	// 调用函数
	fnValue.Call(callArgs)

	// 返回捕获到的输出
	return outputBuffer.String(), nil
}

// executeShell 执行shell类型的任务
func (m *TaskManager) executeShell(task *models.CronTask) (string, error) {
	// 获取参数
	args, err := task.GetArgs()
	if err != nil {
		return "", fmt.Errorf("解析参数失败: %v", err)
	}

	// 构建命令
	command := task.Target
	if len(args) > 0 {
		// 将参数转换为字符串并添加到命令中
		argsStr, err := json.Marshal(args)
		if err != nil {
			return "", fmt.Errorf("序列化参数失败: %v", err)
		}
		command = fmt.Sprintf("%s %s", command, string(argsStr))
	}

	// 执行命令
	cmd := exec.Command("bash", "-c", command)
	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	if err != nil {
		return outputStr, fmt.Errorf("执行命令失败: %v", err)
	}

	log.Printf("[CronTask] 命令执行输出: %s", outputStr)
	return outputStr, nil
}

// GetEntries 获取所有cron条目
func (m *TaskManager) GetEntries() []cron.Entry {
	if !m.isRunning {
		return nil
	}
	return m.cron.Entries()
}

// GetTaskStatus 获取任务状态
func (m *TaskManager) GetTaskStatus(id int) (bool, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	_, exists := m.entryMap[id]
	return exists, nil
}

// checkDependencies 检查任务依赖关系
func (m *TaskManager) checkDependencies(task *models.CronTask) error {
	// 获取依赖任务ID
	deps, err := task.GetDependencies()
	if err != nil {
		return fmt.Errorf("获取依赖任务失败: %v", err)
	}

	if len(deps) == 0 {
		return nil
	}

	// 检查每个依赖任务的状态
	for _, depID := range deps {
		// 获取依赖任务
		depTask, err := models.GetTaskByID(depID)
		if err != nil {
			return fmt.Errorf("获取依赖任务失败 (ID: %d): %v", depID, err)
		}

		// 检查依赖任务的最后运行状态
		if depTask.LastStatus != 1 {
			return fmt.Errorf("依赖任务 '%s' (ID: %d) 未成功执行", depTask.Name, depID)
		}

		// 检查依赖任务的最后运行时间
		if depTask.LastRunTime.IsZero() {
			return fmt.Errorf("依赖任务 '%s' (ID: %d) 从未运行", depTask.Name, depID)
		}

		// 检查依赖任务的最后运行时间是否在当前任务的上次运行时间之后
		if !task.LastRunTime.IsZero() && depTask.LastRunTime.Before(task.LastRunTime) {
			return fmt.Errorf("依赖任务 '%s' (ID: %d) 在当前任务上次运行后未执行", depTask.Name, depID)
		}
	}

	return nil
}

// retryTask 重试任务
func (m *TaskManager) retryTask(task *models.CronTask, retryCount int) {
	// 检查重试次数
	if retryCount > task.RetryTimes {
		log.Printf("[CronTask] 任务重试次数已达上限: %s (ID: %d), 重试次数: %d/%d", task.Name, task.ID, retryCount-1, task.RetryTimes)
		return
	}

	// 等待重试间隔
	retryInterval := task.RetryInterval
	if retryInterval <= 0 {
		retryInterval = 5 // 默认5秒
	}

	log.Printf("[CronTask] 任务将在 %d 秒后重试: %s (ID: %d), 重试次数: %d/%d", retryInterval, task.Name, task.ID, retryCount, task.RetryTimes)
	time.Sleep(time.Duration(retryInterval) * time.Second)

	// 重新获取任务信息，确保使用最新的任务配置
	updatedTask, err := models.GetTaskByID(task.ID)
	if err != nil {
		log.Printf("[CronTask] 获取任务信息失败: %v", err)
		return
	}

	// 检查任务是否仍然启用
	if updatedTask.Status != 1 {
		log.Printf("[CronTask] 任务已禁用，取消重试: %s (ID: %d)", updatedTask.Name, updatedTask.ID)
		return
	}

	// 执行任务
	log.Printf("[CronTask] 开始重试任务: %s (ID: %d), 重试次数: %d/%d", updatedTask.Name, updatedTask.ID, retryCount, updatedTask.RetryTimes)

	// 创建任务日志
	startTime := time.Now()
	taskLog := &models.CronTaskLog{
		TaskID:    updatedTask.ID,
		TaskName:  updatedTask.Name,
		StartTime: startTime,
		CreatedAt: time.Now(),
		Output:    fmt.Sprintf("重试执行 (%d/%d)", retryCount, updatedTask.RetryTimes),
	}

	// 执行任务
	var output string
	var execErr error

	switch updatedTask.Type {
	case "function":
		output, execErr = m.executeFunction(updatedTask)
	case "shell":
		output, execErr = m.executeShell(updatedTask)
	default:
		execErr = fmt.Errorf("不支持的任务类型: %s", updatedTask.Type)
	}

	// 记录执行结果
	endTime := time.Now()
	duration := endTime.Sub(startTime).Milliseconds()

	// 更新任务日志
	taskLog.EndTime = endTime
	taskLog.Duration = duration
	taskLog.Output += "\n" + output

	// 处理执行结果
	var taskStatus int
	if execErr != nil {
		log.Printf("[CronTask] 重试任务执行失败: %s (ID: %d), 错误: %v", updatedTask.Name, updatedTask.ID, execErr)
		taskLog.Status = 0
		taskLog.Error = execErr.Error()
		taskStatus = 0

		// 继续重试
		if retryCount < updatedTask.RetryTimes {
			go m.retryTask(updatedTask, retryCount+1)
		}
	} else {
		log.Printf("[CronTask] 重试任务执行成功: %s (ID: %d), 耗时: %v", updatedTask.Name, updatedTask.ID, time.Since(startTime))
		taskLog.Status = 1
		taskStatus = 1
	}

	// 保存任务日志
	if err := models.AddTaskLog(taskLog); err != nil {
		log.Printf("[CronTask] 保存重试任务日志失败: %v", err)
	}

	// 更新任务状态
	if err := models.UpdateTaskLastStatus(updatedTask.ID, taskStatus); err != nil {
		log.Printf("[CronTask] 更新重试任务状态失败: %v", err)
	}
}

// GetRegisteredFunctions 获取所有注册的函数
func (m *TaskManager) GetRegisteredFunctions() map[string]FunctionInfo {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	// 创建一个副本返回
	result := make(map[string]FunctionInfo)
	for name, info := range m.functionInfoMap {
		result[name] = info
	}

	return result
}

// ValidateSpec 验证cron表达式
func (m *TaskManager) ValidateSpec(spec string) (cron.Schedule, error) {
	parser := cron.NewParser(
		cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)
	return parser.Parse(spec)
}
