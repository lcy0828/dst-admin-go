package automation

import "strings"

const (
	playerManagementGroupName       = "player-management"
	playerRefreshSystemGroupName    = "player-refresh-system"
	defaultPlayerRefreshTaskName    = "自动刷新玩家数据"
	defaultPlayerRefreshSchedule    = "* * * * *"
	defaultPlayerRefreshDescription = "系统默认任务：每分钟刷新所有运行中分片的玩家状态"
	legacyPlayerRefreshSchedule     = "*/30 * * * * *"
	legacyPlayerRefreshDescription  = "系统默认任务：每 30 秒刷新所有运行中分片的玩家状态"
)

// EnsureDefaultPlayerRefresh creates the built-in collector or migrates the
// old built-in 30-second collector once. Any later user changes are retained.
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

	var current Task
	for _, task := range tasks {
		if current.ID == "" && isDefaultPlayerRefresh(task) {
			current = task
		}
	}
	if current.ID != "" && !needsDefaultPlayerRefreshMigration(current) {
		return current, false, nil
	}

	group, _, err := s.ensurePlayerRefreshGroup(roomID, groups, groupsByID[current.GroupID])
	if err != nil {
		return Task{}, false, err
	}
	if current.ID != "" {
		current.GroupID = group.ID
		current.Description = defaultPlayerRefreshDescription
		current.Enabled = true
		current.Schedule = defaultPlayerRefreshSchedule
		updated, updateErr := s.updateTask(current)
		return updated, true, updateErr
	}

	task, err := s.CreateTask(roomID, TaskInput{
		GroupID: group.ID, Name: defaultPlayerRefreshTaskName, Description: defaultPlayerRefreshDescription,
		Enabled: true, Schedule: defaultPlayerRefreshSchedule, Timezone: "Asia/Shanghai",
		Action: ActionPlayerRefresh, WorldIDs: []string{}, Parameters: map[string]interface{}{},
		TimeoutSeconds: 60, RetryInterval: 60,
	})
	return task, err == nil, err
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
		Name: name, Description: "用于定时刷新玩家状态。", Type: "system", Enabled: true,
	})
	return group, err == nil, err
}

func (s *Service) enableGroup(group Group) (Group, bool, error) {
	group, err := s.UpdateGroup(group.RoomID, group.ID, GroupInput{
		Name: group.Name, Description: group.Description, Type: group.Type, Enabled: true, ExpectedRevision: group.Revision,
	})
	return group, err == nil, err
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

func needsDefaultPlayerRefreshMigration(task Task) bool {
	return isDefaultPlayerRefresh(task) && task.Schedule == legacyPlayerRefreshSchedule && task.Description == legacyPlayerRefreshDescription
}

func isPlayerManagementGroup(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case playerManagementGroupName, "player management", "玩家管理":
		return true
	default:
		return false
	}
}
