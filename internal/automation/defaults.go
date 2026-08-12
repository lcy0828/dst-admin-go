package automation

import "strings"

const (
	playerManagementGroupName       = "player-management"
	playerRefreshSystemGroupName    = "player-refresh-system"
	defaultPlayerRefreshTaskName    = "自动刷新玩家数据"
	defaultPlayerRefreshSchedule    = "*/30 * * * * *"
	defaultPlayerRefreshDescription = "系统默认任务：每 30 秒刷新所有运行中分片的玩家状态"
)

// EnsureDefaultPlayerRefresh guarantees one schedulable all-world refresh.
func (s *Service) EnsureDefaultPlayerRefresh(roomID string) (Task, bool, error) {
	s.defaultsMu.Lock()
	defer s.defaultsMu.Unlock()

	tasks, err := s.Tasks(roomID)
	if err != nil {
		return Task{}, false, err
	}
	groups, err := s.Groups(roomID)
	if err != nil {
		return Task{}, false, err
	}
	groupsByID := make(map[string]Group, len(groups))
	for _, group := range groups {
		groupsByID[group.ID] = group
	}

	changed := false
	for index := range tasks {
		if !isLegacyPlayerRefresh(tasks[index]) || !tasks[index].Enabled {
			continue
		}
		tasks[index], err = s.setTaskEnabled(tasks[index], false)
		if err != nil {
			return Task{}, changed, err
		}
		changed = true
	}

	for _, task := range tasks {
		group := groupsByID[task.GroupID]
		if task.Action == ActionPlayerRefresh && len(task.WorldIDs) == 0 && task.Enabled && group.Enabled {
			return task, changed, nil
		}
	}

	var defaultTask Task
	for _, task := range tasks {
		if isDefaultPlayerRefresh(task) {
			defaultTask = task
			break
		}
	}

	group, groupChanged, err := s.ensurePlayerRefreshGroup(roomID, groups, groupsByID[defaultTask.GroupID])
	if err != nil {
		return Task{}, changed, err
	}
	changed = changed || groupChanged

	if defaultTask.ID != "" {
		if defaultTask.GroupID != group.ID {
			defaultTask.GroupID = group.ID
			changed = true
		}
		if !defaultTask.Enabled {
			defaultTask.Enabled = true
			changed = true
		}
		if changed {
			defaultTask, err = s.updateTask(defaultTask)
			if err != nil {
				return Task{}, true, err
			}
		}
		return defaultTask, changed, nil
	}

	task, err := s.CreateTask(roomID, TaskInput{
		GroupID: group.ID, Name: defaultPlayerRefreshTaskName, Description: defaultPlayerRefreshDescription,
		Enabled: true, Schedule: defaultPlayerRefreshSchedule, Timezone: "Asia/Shanghai",
		Action: ActionPlayerRefresh, WorldIDs: []string{}, Parameters: map[string]interface{}{},
		TimeoutSeconds: 300, RetryInterval: 60,
	})
	if err != nil {
		return Task{}, changed, err
	}
	return task, true, nil
}

func (s *Service) ensurePlayerRefreshGroup(roomID string, groups []Group, current Group) (Group, bool, error) {
	if current.ID != "" && current.Enabled {
		return current, false, nil
	}
	if current.ID != "" && current.Type == "system" {
		return s.enableGroup(current)
	}

	for _, candidate := range groups {
		if isPlayerManagementGroup(candidate.Name) && candidate.Enabled {
			return candidate, false, nil
		}
	}
	for _, candidate := range groups {
		if (isPlayerManagementGroup(candidate.Name) || candidate.Name == playerRefreshSystemGroupName) && candidate.Type == "system" {
			if candidate.Enabled {
				return candidate, false, nil
			}
			return s.enableGroup(candidate)
		}
	}

	name := playerManagementGroupName
	for _, candidate := range groups {
		if isPlayerManagementGroup(candidate.Name) {
			name = playerRefreshSystemGroupName
			break
		}
	}
	group, err := s.CreateGroup(roomID, GroupInput{
		Name: name, Description: "用于执行定时玩家信息刷新。", Type: "system", Enabled: true,
	})
	return group, err == nil, err
}

func (s *Service) enableGroup(group Group) (Group, bool, error) {
	group, err := s.UpdateGroup(group.RoomID, group.ID, GroupInput{
		Name: group.Name, Description: group.Description, Type: group.Type, Enabled: true, ExpectedRevision: group.Revision,
	})
	return group, err == nil, err
}

func (s *Service) setTaskEnabled(task Task, enabled bool) (Task, error) {
	task.Enabled = enabled
	return s.updateTask(task)
}

func (s *Service) updateTask(task Task) (Task, error) {
	return s.UpdateTask(task.RoomID, task.ID, TaskInput{
		GroupID: task.GroupID, Name: task.Name, Description: task.Description, Enabled: task.Enabled,
		Schedule: task.Schedule, Timezone: task.Timezone, Action: task.Action, WorldIDs: task.WorldIDs,
		Parameters: task.Parameters, TimeoutSeconds: task.TimeoutSeconds, RetryTimes: task.RetryTimes,
		RetryInterval: task.RetryInterval, Dependencies: task.Dependencies, ExpectedRevision: task.Revision,
	})
}

func isDefaultPlayerRefresh(task Task) bool {
	return task.Action == ActionPlayerRefresh && len(task.WorldIDs) == 0 && task.Name == defaultPlayerRefreshTaskName
}

func isLegacyPlayerRefresh(task Task) bool {
	return task.Action == ActionPlayerRefresh && strings.Contains(task.Description, "由旧版任务 #")
}

func isPlayerManagementGroup(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case playerManagementGroupName, "player management", "玩家管理":
		return true
	default:
		return false
	}
}
