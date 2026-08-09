package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dont/internal/rooms"

	"github.com/jinzhu/gorm"
)

const legacyMigrationGroupName = "旧版任务迁移"

type LegacyMigrationCatalog interface {
	List() ([]rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type LegacyMigrationSkip struct {
	ID     int
	Name   string
	Reason string
}

type LegacyMigrationReport struct {
	Examined int
	Migrated int
	Existing int
	Skipped  []LegacyMigrationSkip
}

type legacyCronTaskRecord struct {
	ID            int
	Name          string
	Description   string
	Spec          string
	Type          string
	Target        string
	Args          string
	Dependencies  string
	Timeout       int
	RetryTimes    int
	RetryInterval int
	Status        int
}

type legacyTaskCandidate struct {
	record  legacyCronTaskRecord
	room    rooms.Room
	world   rooms.World
	action  Action
	marker  string
	details string
}

func MigrateLegacyCronTasks(db *gorm.DB, tablePrefix string, catalog LegacyMigrationCatalog, service *Service) (LegacyMigrationReport, error) {
	report := LegacyMigrationReport{Skipped: []LegacyMigrationSkip{}}
	if db == nil || catalog == nil || service == nil {
		return report, errors.New("legacy cron migration dependencies are required")
	}
	table := strings.TrimSpace(tablePrefix) + "cron_task"
	if !db.HasTable(table) {
		return report, nil
	}

	var records []legacyCronTaskRecord
	if err := db.Table(table).Order("id ASC").Find(&records).Error; err != nil {
		return report, fmt.Errorf("read legacy cron tasks: %w", err)
	}
	report.Examined = len(records)
	availableRooms, err := catalog.List()
	if err != nil {
		return report, fmt.Errorf("list rooms for legacy cron migration: %w", err)
	}

	for _, record := range records {
		candidate, reason := legacyCandidate(record, availableRooms, catalog)
		if reason != "" {
			report.Skipped = append(report.Skipped, LegacyMigrationSkip{ID: record.ID, Name: record.Name, Reason: reason})
			continue
		}

		tasks, taskErr := service.Tasks(candidate.room.ID)
		if taskErr != nil {
			report.Skipped = append(report.Skipped, LegacyMigrationSkip{ID: record.ID, Name: record.Name, Reason: taskErr.Error()})
			continue
		}
		if existing, found := legacyTaskByName(tasks, record.Name); found {
			if strings.Contains(existing.Description, candidate.marker) {
				report.Existing++
			} else {
				report.Skipped = append(report.Skipped, LegacyMigrationSkip{ID: record.ID, Name: record.Name, Reason: "目标房间已有同名任务，未覆盖"})
			}
			continue
		}

		group, groupErr := ensureLegacyMigrationGroup(service, candidate.room.ID)
		if groupErr != nil {
			report.Skipped = append(report.Skipped, LegacyMigrationSkip{ID: record.ID, Name: record.Name, Reason: groupErr.Error()})
			continue
		}
		_, createErr := service.CreateTask(candidate.room.ID, TaskInput{
			GroupID: group.ID, Name: record.Name, Description: candidate.details,
			Enabled: record.Status == 1, Schedule: record.Spec, Timezone: "Asia/Shanghai",
			Action: candidate.action, WorldIDs: []string{candidate.world.ID}, Parameters: map[string]interface{}{},
			TimeoutSeconds: legacyTimeout(record.Timeout), RetryTimes: record.RetryTimes,
			RetryInterval: legacyRetryInterval(record.RetryInterval), Dependencies: []string{},
		})
		if createErr != nil {
			report.Skipped = append(report.Skipped, LegacyMigrationSkip{ID: record.ID, Name: record.Name, Reason: createErr.Error()})
			continue
		}
		report.Migrated++
	}
	return report, nil
}

func legacyCandidate(record legacyCronTaskRecord, availableRooms []rooms.Room, catalog LegacyMigrationCatalog) (legacyTaskCandidate, string) {
	if record.Type != "function" {
		return legacyTaskCandidate{}, "仅支持迁移旧版受控函数任务"
	}
	if strings.TrimSpace(record.Dependencies) != "" && strings.TrimSpace(record.Dependencies) != "[]" {
		return legacyTaskCandidate{}, "旧版数字依赖关系无法安全映射"
	}
	action, supported := mapLegacyAction(record.Target)
	if !supported {
		return legacyTaskCandidate{}, fmt.Sprintf("旧动作 %q 没有安全的新版映射", record.Target)
	}
	if strings.TrimSpace(record.Name) == "" || len([]rune(record.Name)) > 80 {
		return legacyTaskCandidate{}, "任务名称不符合新版 1-80 字符限制"
	}
	if record.Timeout != 0 && (record.Timeout < 5 || record.Timeout > 3600) {
		return legacyTaskCandidate{}, "超时不符合新版 5-3600 秒限制"
	}
	if record.RetryTimes < 0 || record.RetryTimes > 10 || (record.RetryInterval != 0 && (record.RetryInterval < 1 || record.RetryInterval > 3600)) {
		return legacyTaskCandidate{}, "重试设置超出新版安全范围"
	}

	var args []interface{}
	if err := json.Unmarshal([]byte(record.Args), &args); err != nil || len(args) < 2 {
		return legacyTaskCandidate{}, "参数必须包含房间和世界名称"
	}
	roomName, roomOK := args[0].(string)
	worldName, worldOK := args[1].(string)
	if !roomOK || !worldOK || strings.TrimSpace(roomName) == "" || strings.TrimSpace(worldName) == "" {
		return legacyTaskCandidate{}, "房间或世界参数不是有效字符串"
	}

	var room rooms.Room
	for _, item := range availableRooms {
		if item.Managed && (item.DirectoryName == roomName || item.Name == roomName) {
			room = item
			break
		}
	}
	if room.ID == "" {
		return legacyTaskCandidate{}, fmt.Sprintf("目标房间 %q 不存在或尚未接管", roomName)
	}
	worlds, err := catalog.Worlds(room.ID)
	if err != nil {
		return legacyTaskCandidate{}, fmt.Sprintf("读取目标房间世界失败：%v", err)
	}
	var world rooms.World
	for _, item := range worlds {
		if item.DirectoryName == worldName || item.Name == worldName {
			world = item
			break
		}
	}
	if world.ID == "" {
		return legacyTaskCandidate{}, fmt.Sprintf("目标世界 %q 不存在", worldName)
	}

	marker := fmt.Sprintf("由旧版任务 #%d 迁移", record.ID)
	details := strings.TrimSpace(record.Description)
	if details == "" {
		details = marker
	} else {
		details += "（" + marker + "）"
	}
	if len([]rune(details)) > 300 {
		runes := []rune(details)
		details = string(runes[:300-len([]rune(marker))-2]) + "（" + marker + "）"
	}
	return legacyTaskCandidate{record: record, room: room, world: world, action: action, marker: marker, details: details}, ""
}

func mapLegacyAction(target string) (Action, bool) {
	switch strings.TrimSpace(target) {
	case "read_player_config":
		return ActionPlayerRefresh, true
	case "read_world_state":
		return ActionWorldStateRefresh, true
	default:
		return "", false
	}
}

func ensureLegacyMigrationGroup(service *Service, roomID string) (Group, error) {
	groups, err := service.Groups(roomID)
	if err != nil {
		return Group{}, err
	}
	for _, group := range groups {
		if group.Name == legacyMigrationGroupName {
			return group, nil
		}
	}
	return service.CreateGroup(roomID, GroupInput{
		Name: legacyMigrationGroupName, Description: "从旧版 Cron 安全迁移的任务", Type: "world", Enabled: true,
	})
}

func legacyTaskByName(tasks []Task, name string) (Task, bool) {
	for _, task := range tasks {
		if task.Name == name {
			return task, true
		}
	}
	return Task{}, false
}

func legacyTimeout(value int) int {
	if value == 0 {
		return 300
	}
	return value
}

func legacyRetryInterval(value int) int {
	if value == 0 {
		return 60
	}
	return value
}
