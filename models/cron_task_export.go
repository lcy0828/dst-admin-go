package models

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// TaskExportData 任务导出数据结构
type TaskExportData struct {
	Version     string          `json:"version"`     // 导出版本
	ExportTime  time.Time       `json:"export_time"` // 导出时间
	Groups      []CronTaskGroup `json:"groups"`      // 任务组
	Tasks       []CronTask      `json:"tasks"`       // 任务
	Description string          `json:"description"` // 描述信息
}

// ExportTasks 导出任务到文件
func ExportTasks(filePath string, description string) error {
	// 获取所有任务组
	groups, err := GetAllTaskGroups()
	if err != nil {
		return fmt.Errorf("获取任务组失败: %v", err)
	}

	// 获取所有任务
	tasks, err := GetAllTasks()
	if err != nil {
		return fmt.Errorf("获取任务失败: %v", err)
	}

	// 创建导出数据
	exportData := TaskExportData{
		Version:     "1.0",
		ExportTime:  time.Now(),
		Groups:      groups,
		Tasks:       tasks,
		Description: description,
	}

	// 转换为JSON
	data, err := json.MarshalIndent(exportData, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化数据失败: %v", err)
	}

	// 写入文件
	err = os.WriteFile(filePath, data, 0644)
	if err != nil {
		return fmt.Errorf("写入文件失败: %v", err)
	}

	return nil
}

// ImportTasks 从文件导入任务
func ImportTasks(filePath string, overwrite bool) error {
	// 读取文件
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("读取文件失败: %v", err)
	}

	// 解析JSON
	var importData TaskExportData
	err = json.Unmarshal(data, &importData)
	if err != nil {
		return fmt.Errorf("解析JSON失败: %v", err)
	}

	// 开启事务
	tx := db.Begin()

	// 如果选择覆盖，则先清空现有数据
	if overwrite {
		if err := tx.Delete(&CronTask{}).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("清空任务表失败: %v", err)
		}
		if err := tx.Delete(&CronTaskGroup{}).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("清空任务组表失败: %v", err)
		}
	}

	// 导入任务组
	groupIDMap := make(map[int]int) // 旧ID到新ID的映射
	for _, group := range importData.Groups {
		// 检查是否已存在同名任务组
		var existingGroup CronTaskGroup
		result := tx.Where("name = ?", group.Name).First(&existingGroup)

		if result.Error == nil && !overwrite {
			// 已存在且不覆盖，记录ID映射
			groupIDMap[group.ID] = existingGroup.ID
			continue
		}

		// 保存旧ID
		oldID := group.ID

		// 重置ID，让数据库自动生成
		if !overwrite {
			group.ID = 0
		}

		// 更新时间戳
		group.CreatedAt = time.Now()
		group.UpdatedAt = time.Now()

		// 创建或更新任务组
		if err := tx.Save(&group).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("保存任务组失败: %v", err)
		}

		// 记录ID映射
		groupIDMap[oldID] = group.ID
	}

	// 导入任务
	taskIDMap := make(map[int]int) // 旧ID到新ID的映射
	for _, task := range importData.Tasks {
		// 检查是否已存在同名任务
		var existingTask CronTask
		result := tx.Where("name = ?", task.Name).First(&existingTask)

		if result.Error == nil && !overwrite {
			// 已存在且不覆盖，记录ID映射
			taskIDMap[task.ID] = existingTask.ID
			continue
		}

		// 保存旧ID
		oldID := task.ID

		// 重置ID，让数据库自动生成
		if !overwrite {
			task.ID = 0
		}

		// 更新任务组ID
		if newGroupID, ok := groupIDMap[task.GroupID]; ok {
			task.GroupID = newGroupID
		}

		// 更新时间戳
		task.CreatedAt = time.Now()
		task.UpdatedAt = time.Now()

		// 创建或更新任务
		if err := tx.Save(&task).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("保存任务失败: %v", err)
		}

		// 记录ID映射
		taskIDMap[oldID] = task.ID
	}

	// 更新任务依赖关系
	for _, task := range importData.Tasks {
		// 获取新的任务ID
		newTaskID, ok := taskIDMap[task.ID]
		if !ok {
			continue
		}

		// 获取依赖关系
		deps, err := task.GetDependencies()
		if err != nil || len(deps) == 0 {
			continue
		}

		// 更新依赖关系
		newDeps := make([]int, 0, len(deps))
		for _, dep := range deps {
			if newDep, ok := taskIDMap[dep]; ok {
				newDeps = append(newDeps, newDep)
			}
		}

		// 保存新的依赖关系
		var newTask CronTask
		if err := tx.First(&newTask, newTaskID).Error; err != nil {
			continue
		}

		if err := newTask.SetDependencies(newDeps); err != nil {
			continue
		}

		if err := tx.Save(&newTask).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("更新任务依赖关系失败: %v", err)
		}
	}

	// 提交事务
	return tx.Commit().Error
}
