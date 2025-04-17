package models

import (
	"encoding/json"
	"fmt"
	"time"
)

// CronTaskGroup 定时任务组
type CronTaskGroup struct {
	ID          int       `gorm:"primary_key" json:"id"`
	Name        string    `json:"name"`        // 组名称
	Description string    `json:"description"` // 组描述
	Type        string    `json:"type"`        // 组类型：system(系统)、world(世界)、custom(自定义)
	Status      int       `json:"status"`      // 状态：0-禁用，1-启用
	CreatedAt   time.Time `json:"created_at"`  // 创建时间
	UpdatedAt   time.Time `json:"updated_at"`  // 更新时间
}

// CronTask 定时任务模型
type CronTask struct {
	ID            int       `gorm:"primary_key" json:"id"`
	Name          string    `json:"name"`           // 任务名称
	Description   string    `json:"description"`    // 任务描述
	GroupID       int       `json:"group_id"`       // 所属组ID
	Spec          string    `json:"spec"`           // cron表达式
	Type          string    `json:"type"`           // 任务类型：function(内置函数)、shell(shell命令)
	Target        string    `json:"target"`         // 目标：函数名或shell命令
	Args          string    `json:"args"`           // 参数，JSON格式
	Dependencies  string    `json:"dependencies"`   // 依赖的任务ID，JSON格式的数组
	Timeout       int       `json:"timeout"`        // 超时时间（秒），0表示无超时
	RetryTimes    int       `json:"retry_times"`    // 重试次数，0表示不重试
	RetryInterval int       `json:"retry_interval"` // 重试间隔（秒）
	Status        int       `json:"status"`         // 状态：0-禁用，1-启用
	LastRunTime   time.Time `json:"last_run_time"`  // 上次运行时间
	LastStatus    int       `json:"last_status"`    // 上次运行状态：0-失败，1-成功，-1-未运行
	CreatedAt     time.Time `json:"created_at"`     // 创建时间
	UpdatedAt     time.Time `json:"updated_at"`     // 更新时间
}

// GetArgs 获取参数
func (t *CronTask) GetArgs() ([]interface{}, error) {
	if t.Args == "" {
		return nil, nil
	}

	var args []interface{}
	err := json.Unmarshal([]byte(t.Args), &args)
	if err != nil {
		return nil, err
	}

	return args, nil
}

// SetArgs 设置参数
func (t *CronTask) SetArgs(args []interface{}) error {
	if args == nil {
		t.Args = ""
		return nil
	}

	data, err := json.Marshal(args)
	if err != nil {
		return err
	}

	t.Args = string(data)
	return nil
}

// GetDependencies 获取依赖任务ID
func (t *CronTask) GetDependencies() ([]int, error) {
	if t.Dependencies == "" {
		return nil, nil
	}

	var deps []int
	err := json.Unmarshal([]byte(t.Dependencies), &deps)
	if err != nil {
		return nil, err
	}

	return deps, nil
}

// SetDependencies 设置依赖任务ID
func (t *CronTask) SetDependencies(deps []int) error {
	if deps == nil || len(deps) == 0 {
		t.Dependencies = ""
		return nil
	}

	data, err := json.Marshal(deps)
	if err != nil {
		return err
	}

	t.Dependencies = string(data)
	return nil
}

// HasDependency 检查是否有依赖关系
func (t *CronTask) HasDependency() bool {
	return t.Dependencies != "" && t.Dependencies != "[]"
}

// IsDependencyOf 检查是否是某个任务的依赖
func (t *CronTask) IsDependencyOf(taskID int) (bool, error) {
	deps, err := t.GetDependencies()
	if err != nil {
		return false, err
	}

	for _, dep := range deps {
		if dep == taskID {
			return true, nil
		}
	}

	return false, nil
}

// GetTaskByID 根据ID获取任务
func GetTaskByID(id int) (*CronTask, error) {
	var task CronTask
	err := db.Where("id = ?", id).First(&task).Error
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// GetAllTasks 获取所有任务
func GetAllTasks() ([]CronTask, error) {
	var tasks []CronTask
	err := db.Find(&tasks).Error
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// GetEnabledTasks 获取所有启用的任务
func GetEnabledTasks() ([]CronTask, error) {
	var tasks []CronTask
	err := db.Where("status = ?", 1).Find(&tasks).Error
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// AddTask 添加任务
func AddTask(task *CronTask) error {
	return db.Create(task).Error
}

// UpdateTask 更新任务
func UpdateTask(task *CronTask) error {
	return db.Save(task).Error
}

// DeleteTask 删除任务
func DeleteTask(id int) error {
	return db.Delete(&CronTask{}, id).Error
}

// UpdateTaskStatus 更新任务状态
func UpdateTaskStatus(id int, status int) error {
	return db.Model(&CronTask{}).Where("id = ?", id).Update("status", status).Error
}

// UpdateLastRunTime 更新任务最后运行时间
func UpdateLastRunTime(id int, lastRunTime time.Time) error {
	return db.Model(&CronTask{}).Where("id = ?", id).Update("last_run_time", lastRunTime).Error
}

// UpdateTaskLastStatus 更新任务最后运行状态
func UpdateTaskLastStatus(id int, status int) error {
	return db.Model(&CronTask{}).Where("id = ?", id).Update("last_status", status).Error
}

// GetTasksByGroupID 根据组ID获取任务列表
func GetTasksByGroupID(groupID int) ([]CronTask, error) {
	var tasks []CronTask
	err := db.Where("group_id = ?", groupID).Find(&tasks).Error
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// GetTasksByDependency 获取依赖指定任务的所有任务
func GetTasksByDependency(taskID int) ([]CronTask, error) {
	var tasks []CronTask
	// 使用LIKE查询依赖关系，这里的实现可能不是最高效的，但对于小规模数据来说足够了
	err := db.Where("dependencies LIKE ?", "%"+fmt.Sprintf("%d", taskID)+"%").Find(&tasks).Error
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// GetTaskGroup 根据ID获取任务组
func GetTaskGroup(id int) (*CronTaskGroup, error) {
	var group CronTaskGroup
	err := db.Where("id = ?", id).First(&group).Error
	if err != nil {
		return nil, err
	}
	return &group, nil
}

// GetAllTaskGroups 获取所有任务组
func GetAllTaskGroups() ([]CronTaskGroup, error) {
	var groups []CronTaskGroup
	err := db.Find(&groups).Error
	if err != nil {
		return nil, err
	}
	return groups, nil
}

// GetEnabledTaskGroups 获取所有启用的任务组
func GetEnabledTaskGroups() ([]CronTaskGroup, error) {
	var groups []CronTaskGroup
	err := db.Where("status = ?", 1).Find(&groups).Error
	if err != nil {
		return nil, err
	}
	return groups, nil
}

// AddTaskGroup 添加任务组
func AddTaskGroup(group *CronTaskGroup) error {
	return db.Create(group).Error
}

// UpdateTaskGroup 更新任务组
func UpdateTaskGroup(group *CronTaskGroup) error {
	return db.Save(group).Error
}

// DeleteTaskGroup 删除任务组
func DeleteTaskGroup(id int) error {
	// 开启事务
	tx := db.Begin()

	// 删除组内所有任务
	if err := tx.Where("group_id = ?", id).Delete(&CronTask{}).Error; err != nil {
		tx.Rollback()
		return err
	}

	// 删除组
	if err := tx.Delete(&CronTaskGroup{}, id).Error; err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}

// UpdateTaskGroupStatus 更新任务组状态
func UpdateTaskGroupStatus(id int, status int) error {
	// 开启事务
	tx := db.Begin()

	// 更新组状态
	if err := tx.Model(&CronTaskGroup{}).Where("id = ?", id).Update("status", status).Error; err != nil {
		tx.Rollback()
		return err
	}

	// 更新组内所有任务的状态
	if err := tx.Model(&CronTask{}).Where("group_id = ?", id).Update("status", status).Error; err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}

// InitCronTaskTable 初始化定时任务表
func InitCronTaskTable() {
	db.AutoMigrate(&CronTask{})
	db.AutoMigrate(&CronTaskGroup{})
	fmt.Println("定时任务表初始化完成")

	// 初始化默认任务组
	initDefaultTaskGroups()
}

// initDefaultTaskGroups 初始化默认任务组
func initDefaultTaskGroups() {
	// 检查是否已存在任务组
	var count int64
	db.Model(&CronTaskGroup{}).Count(&count)
	if count > 0 {
		return
	}

	// 创建默认任务组
	defaultGroups := []CronTaskGroup{
		{
			Name:        "系统任务",
			Description: "系统维护相关的定时任务",
			Type:        "system",
			Status:      1,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		},
		{
			Name:        "世界任务",
			Description: "世界管理相关的定时任务",
			Type:        "world",
			Status:      1,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		},
		{
			Name:        "自定义任务",
			Description: "用户自定义的定时任务",
			Type:        "custom",
			Status:      1,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		},
	}

	// 添加默认任务组
	for _, group := range defaultGroups {
		db.Create(&group)
	}

	fmt.Println("默认任务组初始化完成")
}
