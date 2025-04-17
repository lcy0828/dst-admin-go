package models

import (
	"encoding/json"
	"fmt"
	"time"
)

// TmuxTask tmux任务扩展模型
type TmuxTask struct {
	ID            int       `gorm:"primary_key" json:"id"`
	TaskID        int       `json:"task_id"`        // 关联的CronTask ID
	SessionName   string    `json:"session_name"`   // tmux会话名称
	CommandID     string    `json:"command_id"`     // 命令ID（对于tmux_command类型）
	RawCommand    string    `json:"raw_command"`    // 原始命令（对于tmux_raw_command类型）
	CommandParams string    `json:"command_params"` // 命令参数，JSON格式
	CreatedAt     time.Time `json:"created_at"`     // 创建时间
	UpdatedAt     time.Time `json:"updated_at"`     // 更新时间
}

// TableName 设置表名
// 注意：在GORM中，如果模型定义了TableName方法，那么DefaultTableNameHandler将不会被应用
// 因此需要手动添加表前缀
func (TmuxTask) TableName() string {
	return "dont_tmux_task"
}

// GetCommandParams 获取命令参数
func (t *TmuxTask) GetCommandParams() ([]string, error) {
	if t.CommandParams == "" {
		return nil, nil
	}

	var params []string
	err := json.Unmarshal([]byte(t.CommandParams), &params)
	if err != nil {
		return nil, err
	}

	return params, nil
}

// SetCommandParams 设置命令参数
func (t *TmuxTask) SetCommandParams(params []string) error {
	if params == nil {
		t.CommandParams = ""
		return nil
	}

	data, err := json.Marshal(params)
	if err != nil {
		return err
	}

	t.CommandParams = string(data)
	return nil
}

// CreateTmuxTask 创建tmux任务
func CreateTmuxTask(task *TmuxTask) error {
	return db.Create(task).Error
}

// GetTmuxTaskByTaskID 根据任务ID获取tmux任务
func GetTmuxTaskByTaskID(taskID int) (*TmuxTask, error) {
	var task TmuxTask
	err := db.Where("task_id = ?", taskID).First(&task).Error
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// UpdateTmuxTask 更新tmux任务
func UpdateTmuxTask(task *TmuxTask) error {
	return db.Save(task).Error
}

// DeleteTmuxTask 删除tmux任务
func DeleteTmuxTask(taskID int) error {
	return db.Where("task_id = ?", taskID).Delete(&TmuxTask{}).Error
}

// InitTmuxTaskTable 初始化tmux任务表
func InitTmuxTaskTable() {
	db.AutoMigrate(&TmuxTask{})
	fmt.Println("tmux任务表初始化完成")
}
