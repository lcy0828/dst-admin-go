package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	scheduleParser    = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
}

type Service struct {
	rooms      RoomCatalog
	store      *Store
	jobs       *jobs.Service
	executor   ActionExecutor
	now        func() time.Time
	defaultsMu sync.Mutex
	activeMu   sync.Mutex
	active     map[string]bool
}

func NewService(roomCatalog RoomCatalog, store *Store, jobService *jobs.Service, executor ActionExecutor) (*Service, error) {
	if roomCatalog == nil || store == nil || jobService == nil || executor == nil {
		return nil, errors.New("automation dependencies are required")
	}
	return &Service{rooms: roomCatalog, store: store, jobs: jobService, executor: executor, now: time.Now, active: make(map[string]bool)}, nil
}

func (s *Service) Groups(roomID string) ([]Group, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return nil, err
	}
	return s.store.Groups(roomID)
}

func (s *Service) CreateGroup(roomID string, input GroupInput) (Group, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return Group{}, err
	}
	input, err := normalizeGroupInput(input, false)
	if err != nil {
		return Group{}, err
	}
	now := s.now().UTC()
	return s.store.CreateGroup(Group{ID: uuid.NewString(), RoomID: roomID, Name: input.Name, Description: input.Description, Type: input.Type, Enabled: input.Enabled, Revision: uuid.NewString(), CreatedAt: now, UpdatedAt: now})
}

func (s *Service) UpdateGroup(roomID, groupID string, input GroupInput) (Group, error) {
	if !validID(groupID) {
		return Group{}, ErrGroupNotFound
	}
	existing, err := s.store.Group(roomID, groupID)
	if err != nil {
		return Group{}, err
	}
	input, err = normalizeGroupInput(input, true)
	if err != nil {
		return Group{}, err
	}
	existing.Name, existing.Description, existing.Type, existing.Enabled = input.Name, input.Description, input.Type, input.Enabled
	existing.Revision, existing.UpdatedAt = uuid.NewString(), s.now().UTC()
	return s.store.UpdateGroup(existing, input.ExpectedRevision)
}

func (s *Service) DeleteGroup(roomID, groupID string) error {
	if !validID(groupID) {
		return ErrGroupNotFound
	}
	return s.store.DeleteGroup(roomID, groupID)
}

func (s *Service) Tasks(roomID string) ([]Task, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return nil, err
	}
	items, err := s.store.Tasks(roomID)
	if err != nil {
		return nil, err
	}
	for index := range items {
		items[index].NextRunAt = nextRun(items[index], s.now())
	}
	return items, nil
}

func (s *Service) Task(roomID, taskID string) (Task, error) {
	if !validID(taskID) {
		return Task{}, ErrTaskNotFound
	}
	task, err := s.store.Task(roomID, taskID)
	if err == nil {
		task.NextRunAt = nextRun(task, s.now())
	}
	return task, err
}

func (s *Service) CreateTask(roomID string, input TaskInput) (Task, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return Task{}, err
	}
	input, err := s.normalizeTaskInput(roomID, input, false)
	if err != nil {
		return Task{}, err
	}
	now := s.now().UTC()
	task := Task{ID: uuid.NewString(), RoomID: roomID, GroupID: input.GroupID, Name: input.Name, Description: input.Description, Enabled: input.Enabled, Schedule: input.Schedule, Timezone: input.Timezone, Action: input.Action, WorldIDs: input.WorldIDs, Parameters: input.Parameters, TimeoutSeconds: input.TimeoutSeconds, RetryTimes: input.RetryTimes, RetryInterval: input.RetryInterval, Dependencies: append([]string(nil), input.Dependencies...), Revision: uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	if err := s.executor.Validate(task); err != nil {
		return Task{}, err
	}
	created, err := s.store.CreateTask(task)
	if err == nil {
		created.NextRunAt = nextRun(created, s.now())
	}
	return created, err
}

func (s *Service) UpdateTask(roomID, taskID string, input TaskInput) (Task, error) {
	existing, err := s.Task(roomID, taskID)
	if err != nil {
		return Task{}, err
	}
	input, err = s.normalizeTaskInput(roomID, input, true)
	if err != nil {
		return Task{}, err
	}
	existing.GroupID, existing.Name, existing.Description, existing.Enabled = input.GroupID, input.Name, input.Description, input.Enabled
	existing.Schedule, existing.Timezone, existing.Action = input.Schedule, input.Timezone, input.Action
	existing.WorldIDs, existing.Parameters, existing.TimeoutSeconds = input.WorldIDs, input.Parameters, input.TimeoutSeconds
	existing.RetryTimes, existing.RetryInterval, existing.Dependencies = input.RetryTimes, input.RetryInterval, append([]string(nil), input.Dependencies...)
	if err := s.validateDependencies(roomID, existing.ID, existing.Dependencies); err != nil {
		return Task{}, err
	}
	existing.Revision, existing.UpdatedAt = uuid.NewString(), s.now().UTC()
	if err := s.executor.Validate(existing); err != nil {
		return Task{}, err
	}
	updated, err := s.store.UpdateTask(existing, input.ExpectedRevision)
	if err == nil {
		updated.NextRunAt = nextRun(updated, s.now())
	}
	return updated, err
}

func (s *Service) DeleteTask(roomID, taskID string) error {
	if !validID(taskID) {
		return ErrTaskNotFound
	}
	s.activeMu.Lock()
	running := s.active[taskID]
	s.activeMu.Unlock()
	if running {
		return ErrTaskRunning
	}
	return s.store.DeleteTask(roomID, taskID)
}

func (s *Service) DeleteRoomTasks(roomID string) error {
	s.defaultsMu.Lock()
	defer s.defaultsMu.Unlock()
	return s.store.DeleteRoomTasks(roomID)
}

func (s *Service) Runs(roomID string, filter RunFilter) (RunList, error) {
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 || (filter.Status != "" && !validRunStatus(filter.Status)) {
		return RunList{}, ErrInvalidInput
	}
	if _, err := s.rooms.Room(roomID); err != nil {
		return RunList{}, err
	}
	return s.store.Runs(roomID, filter)
}

func (s *Service) Run(roomID, runID string) (Run, error) {
	if !validID(runID) {
		return Run{}, ErrRunNotFound
	}
	if _, err := s.rooms.Room(roomID); err != nil {
		return Run{}, err
	}
	return s.store.Run(roomID, runID)
}

func (s *Service) ClearRuns(roomID string, input ClearRunsInput) (ClearRunsResult, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return ClearRunsResult{}, err
	}
	if input.KeepDays < 1 || input.KeepDays > 3650 || (input.Status != "" && !validRunStatus(input.Status)) {
		return ClearRunsResult{}, ErrInvalidInput
	}
	if input.TaskID != "" {
		if !validID(input.TaskID) {
			return ClearRunsResult{}, ErrInvalidInput
		}
		if _, err := s.store.Task(roomID, input.TaskID); err != nil {
			return ClearRunsResult{}, err
		}
	}
	before := s.now().UTC().AddDate(0, 0, -input.KeepDays)
	count, err := s.store.ClearRuns(roomID, before, input.TaskID, input.Status)
	return ClearRunsResult{DeletedCount: count}, err
}

func (s *Service) Stats(roomID string, days int) (Stats, error) {
	if days < 1 || days > 90 {
		return Stats{}, ErrInvalidInput
	}
	if _, err := s.rooms.Room(roomID); err != nil {
		return Stats{}, err
	}
	since := s.now().UTC().AddDate(0, 0, -(days - 1)).Truncate(24 * time.Hour)
	stats, err := s.store.Stats(roomID, since)
	stats.Days = days
	return stats, err
}

func (s *Service) RunTask(roomID, taskID string, trigger Trigger) (jobs.Job, error) {
	task, err := s.store.Task(roomID, taskID)
	if err != nil {
		return jobs.Job{}, err
	}
	if dependency := s.unsatisfiedDependency(task); dependency != "" {
		now := s.now().UTC()
		_, _ = s.store.CreateRun(Run{ID: uuid.NewString(), TaskID: task.ID, TaskName: task.Name, GroupID: task.GroupID, GroupName: task.GroupName, RoomID: task.RoomID, Action: task.Action, Trigger: trigger, Status: RunSkipped, Error: dependency, FinishedAt: &now, CreatedAt: now})
		return jobs.Job{}, ErrDependencies
	}
	s.activeMu.Lock()
	if s.active[task.ID] {
		s.activeMu.Unlock()
		now := s.now().UTC()
		_, _ = s.store.CreateRun(Run{ID: uuid.NewString(), TaskID: task.ID, TaskName: task.Name, GroupID: task.GroupID, GroupName: task.GroupName, RoomID: task.RoomID, Action: task.Action, Trigger: trigger, Status: RunSkipped, Error: "上一轮仍在运行", FinishedAt: &now, CreatedAt: now})
		return jobs.Job{}, ErrTaskRunning
	}
	s.active[task.ID] = true
	s.activeMu.Unlock()
	release := func() {
		s.activeMu.Lock()
		delete(s.active, task.ID)
		s.activeMu.Unlock()
	}
	now := s.now().UTC()
	run, err := s.store.CreateRun(Run{ID: uuid.NewString(), TaskID: task.ID, TaskName: task.Name, GroupID: task.GroupID, GroupName: task.GroupName, RoomID: task.RoomID, Action: task.Action, Trigger: trigger, Status: RunQueued, CreatedAt: now})
	if err != nil {
		release()
		return jobs.Job{}, err
	}
	job, err := s.jobs.SubmitFactory("automation.run", task.RoomID, firstWorld(task.WorldIDs), []jobs.TargetSpec{{ID: task.ID, Name: task.Name}}, func(job jobs.Job) jobs.Runner {
		_ = s.store.AttachJob(run.ID, job.ID)
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			defer release()
			started := s.now().UTC()
			_ = s.store.StartRun(run.ID, started)
			taskCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TimeoutSeconds)*time.Second)
			defer cancel()
			var result ExecutionResult
			var executeErr error
			retryCount := 0
			for attempt := 0; attempt <= task.RetryTimes; attempt++ {
				result, executeErr = s.executor.Execute(taskCtx, task, job.ID)
				if executeErr == nil || taskCtx.Err() != nil || attempt == task.RetryTimes {
					break
				}
				retryCount++
				timer := time.NewTimer(time.Duration(task.RetryInterval) * time.Second)
				select {
				case <-taskCtx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					executeErr = taskCtx.Err()
				case <-timer.C:
				}
				if taskCtx.Err() != nil {
					break
				}
			}
			finished := s.now().UTC()
			status := RunSucceeded
			errorMessage := ""
			if executeErr != nil {
				status = RunFailed
				errorMessage = executeErr.Error()
				if errors.Is(executeErr, context.Canceled) || errors.Is(taskCtx.Err(), context.Canceled) {
					status = RunCanceled
				}
			}
			_ = s.store.FinishRun(run.ID, status, result.Message, errorMessage, retryCount, finished)
			if executeErr != nil {
				code := "AUTOMATION_ACTION_FAILED"
				if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
					code, errorMessage = "AUTOMATION_TIMEOUT", "自动化任务执行超时"
				}
				report(jobs.TargetResult{TargetID: task.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: code, Message: errorMessage}})
				return executeErr
			}
			report(jobs.TargetResult{TargetID: task.ID, Status: jobs.StatusSucceeded, Message: result.Message})
			return nil
		}
	})
	if err != nil {
		release()
		_ = s.store.FinishRun(run.ID, RunFailed, "", err.Error(), 0, s.now().UTC())
		return jobs.Job{}, err
	}
	return job, nil
}

func (s *Service) Export(roomID string) (Document, error) {
	groups, err := s.Groups(roomID)
	if err != nil {
		return Document{}, err
	}
	tasks, err := s.Tasks(roomID)
	if err != nil {
		return Document{}, err
	}
	document := Document{Version: 1, Groups: make([]DocumentGroup, 0, len(groups)), Tasks: make([]DocumentTask, 0, len(tasks))}
	for _, group := range groups {
		document.Groups = append(document.Groups, DocumentGroup{Key: group.ID, Name: group.Name, Description: group.Description, Type: group.Type, Enabled: group.Enabled})
	}
	for _, task := range tasks {
		document.Tasks = append(document.Tasks, DocumentTask{Key: task.ID, GroupKey: task.GroupID, Name: task.Name, Description: task.Description, Enabled: task.Enabled, Schedule: task.Schedule, Timezone: task.Timezone, Action: task.Action, WorldIDs: task.WorldIDs, Parameters: task.Parameters, TimeoutSeconds: task.TimeoutSeconds, RetryTimes: task.RetryTimes, RetryInterval: task.RetryInterval, Dependencies: task.Dependencies})
	}
	return document, nil
}

func (s *Service) PreviewImport(roomID string, document Document) (ImportPreview, error) {
	if _, err := s.rooms.Room(roomID); err != nil {
		return ImportPreview{}, err
	}
	issues := validateDocument(document, func(task Task) error { return s.executor.Validate(task) }, roomID)
	digest, err := documentDigest(document)
	if err != nil {
		return ImportPreview{}, err
	}
	return ImportPreview{Digest: digest, Valid: len(issues) == 0, GroupCount: len(document.Groups), TaskCount: len(document.Tasks), Issues: issues}, nil
}

func (s *Service) Import(roomID string, request ImportRequest) (ImportResult, error) {
	preview, err := s.PreviewImport(roomID, request.Document)
	if err != nil {
		return ImportResult{}, err
	}
	if !preview.Valid {
		return ImportResult{}, ErrImportInvalid
	}
	if request.Digest == "" || request.Digest != preview.Digest {
		return ImportResult{}, ErrImportDigest
	}
	return s.store.ImportDocument(roomID, request.Document, request.Replace, s.now().UTC())
}

func (s *Service) normalizeTaskInput(roomID string, input TaskInput, updating bool) (TaskInput, error) {
	input.GroupID = strings.TrimSpace(input.GroupID)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Schedule = strings.Join(strings.Fields(input.Schedule), " ")
	input.Timezone = strings.TrimSpace(input.Timezone)
	if input.Timezone == "" {
		input.Timezone = "Asia/Shanghai"
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 300
	}
	if input.RetryInterval == 0 {
		input.RetryInterval = 60
	}
	if input.Parameters == nil {
		input.Parameters = map[string]interface{}{}
	}
	fields := make(map[string]string)
	if _, err := s.store.Group(roomID, input.GroupID); err != nil {
		fields["groupId"] = "任务组不存在"
	}
	if input.Name == "" || len([]rune(input.Name)) > 80 {
		fields["name"] = "名称必须为 1-80 个字符"
	}
	if len([]rune(input.Description)) > 300 {
		fields["description"] = "说明不能超过 300 个字符"
	}
	if _, err := scheduleParser.Parse(input.Schedule); err != nil {
		fields["schedule"] = "请输入有效的五段或六段 Cron 表达式"
	}
	if _, err := time.LoadLocation(input.Timezone); err != nil || len(input.Timezone) > 64 {
		fields["timezone"] = "时区名称无效"
	}
	if input.TimeoutSeconds < 5 || input.TimeoutSeconds > 3600 {
		fields["timeoutSeconds"] = "超时必须在 5-3600 秒之间"
	}
	if input.RetryTimes < 0 || input.RetryTimes > 10 {
		fields["retryTimes"] = "重试次数必须在 0-10 之间"
	}
	if input.RetryInterval < 1 || input.RetryInterval > 3600 {
		fields["retryIntervalSeconds"] = "重试间隔必须在 1-3600 秒之间"
	}
	if len(input.WorldIDs) > 64 {
		fields["worldIds"] = "世界数量超过上限"
	}
	seen := make(map[string]bool)
	for _, worldID := range input.WorldIDs {
		if !identifierPattern.MatchString(worldID) || seen[worldID] {
			fields["worldIds"] = "世界 ID 无效或重复"
		}
		seen[worldID] = true
	}
	if len(input.Dependencies) > 64 {
		fields["dependencies"] = "依赖任务数量超过上限"
	}
	dependencySeen := make(map[string]bool)
	for _, dependencyID := range input.Dependencies {
		if !validID(dependencyID) || dependencySeen[dependencyID] {
			fields["dependencies"] = "依赖任务 ID 无效或重复"
			continue
		}
		dependencySeen[dependencyID] = true
		if _, err := s.store.Task(roomID, dependencyID); err != nil {
			fields["dependencies"] = "依赖任务不存在"
		}
	}
	if updating && !validID(input.ExpectedRevision) {
		fields["expectedRevision"] = "版本号无效"
	}
	if len(fields) > 0 {
		return TaskInput{}, &FieldError{Fields: fields}
	}
	return input, nil
}

func normalizeGroupInput(input GroupInput, updating bool) (GroupInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Type = normalizeGroupType(input.Type)
	fields := make(map[string]string)
	if input.Name == "" || len([]rune(input.Name)) > 80 {
		fields["name"] = "名称必须为 1-80 个字符"
	}
	if len([]rune(input.Description)) > 300 {
		fields["description"] = "说明不能超过 300 个字符"
	}
	if updating && !validID(input.ExpectedRevision) {
		fields["expectedRevision"] = "版本号无效"
	}
	if len(fields) > 0 {
		return GroupInput{}, &FieldError{Fields: fields}
	}
	return input, nil
}

func nextRun(task Task, now time.Time) *time.Time {
	if !task.Enabled {
		return nil
	}
	location, err := time.LoadLocation(task.Timezone)
	if err != nil {
		return nil
	}
	schedule, err := scheduleParser.Parse(task.Schedule)
	if err != nil {
		return nil
	}
	value := schedule.Next(now.In(location)).UTC()
	return &value
}

func normalizeGroupType(value string) string {
	switch strings.TrimSpace(value) {
	case "system", "world", "custom":
		return strings.TrimSpace(value)
	default:
		return "custom"
	}
}

func (s *Service) validateDependencies(roomID, taskID string, dependencies []string) error {
	for _, dependencyID := range dependencies {
		if dependencyID == taskID {
			return &FieldError{Fields: map[string]string{"dependencies": "任务不能依赖自身"}}
		}
	}
	tasks, err := s.store.Tasks(roomID)
	if err != nil {
		return err
	}
	graph := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		graph[task.ID] = task.Dependencies
	}
	graph[taskID] = dependencies
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, dependencyID := range graph[id] {
			if visit(dependencyID) {
				return true
			}
		}
		visiting[id], visited[id] = false, true
		return false
	}
	if visit(taskID) {
		return &FieldError{Fields: map[string]string{"dependencies": "任务依赖不能形成循环"}}
	}
	return nil
}

func (s *Service) unsatisfiedDependency(task Task) string {
	for _, dependencyID := range task.Dependencies {
		dependency, err := s.store.Task(task.RoomID, dependencyID)
		if err != nil {
			return "依赖任务不存在"
		}
		if dependency.LastRunAt == nil || dependency.LastStatus != RunSucceeded {
			return "依赖任务“" + dependency.Name + "”尚未成功执行"
		}
	}
	return ""
}

func validID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil
}

func validRunStatus(status RunStatus) bool {
	return status == RunQueued || status == RunRunning || status == RunSucceeded || status == RunFailed || status == RunCanceled || status == RunSkipped
}

func firstWorld(worldIDs []string) string {
	if len(worldIDs) == 1 {
		return worldIDs[0]
	}
	return ""
}

func documentDigest(document Document) (string, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateDocument(document Document, validateAction func(Task) error, roomID string) []ImportIssue {
	issues := make([]ImportIssue, 0)
	if document.Version != 1 {
		issues = append(issues, ImportIssue{Path: "version", Message: "仅支持版本 1"})
	}
	if len(document.Groups) > 100 || len(document.Tasks) > 500 {
		issues = append(issues, ImportIssue{Path: "document", Message: "导入内容超过 100 个组或 500 个任务"})
	}
	groupKeys := make(map[string]bool)
	groupNames := make(map[string]bool)
	for index, group := range document.Groups {
		path := fmt.Sprintf("groups[%d]", index)
		if strings.TrimSpace(group.Key) == "" || groupKeys[group.Key] {
			issues = append(issues, ImportIssue{Path: path + ".key", Message: "组 key 为空或重复"})
		}
		groupKeys[group.Key] = true
		name := strings.TrimSpace(group.Name)
		if name == "" || len([]rune(name)) > 80 || groupNames[strings.ToLower(name)] {
			issues = append(issues, ImportIssue{Path: path + ".name", Message: "组名称无效或重复"})
		}
		groupNames[strings.ToLower(name)] = true
	}
	taskNames := make(map[string]bool)
	taskKeys := make(map[string]bool)
	for index, item := range document.Tasks {
		key := strings.TrimSpace(item.Key)
		if key == "" {
			key = fmt.Sprintf("task-%d", index)
		}
		if taskKeys[key] {
			issues = append(issues, ImportIssue{Path: fmt.Sprintf("tasks[%d].key", index), Message: "任务 key 重复"})
		}
		taskKeys[key] = true
	}
	for index, item := range document.Tasks {
		path := fmt.Sprintf("tasks[%d]", index)
		if !groupKeys[item.GroupKey] {
			issues = append(issues, ImportIssue{Path: path + ".groupKey", Message: "引用的任务组不存在"})
		}
		name := strings.TrimSpace(item.Name)
		if name == "" || len([]rune(name)) > 80 || taskNames[strings.ToLower(name)] {
			issues = append(issues, ImportIssue{Path: path + ".name", Message: "任务名称无效或重复"})
		}
		taskNames[strings.ToLower(name)] = true
		for _, dependency := range item.Dependencies {
			if !taskKeys[dependency] {
				issues = append(issues, ImportIssue{Path: path + ".dependencies", Message: "引用的依赖任务不存在"})
			}
		}
		if _, err := scheduleParser.Parse(strings.Join(strings.Fields(item.Schedule), " ")); err != nil {
			issues = append(issues, ImportIssue{Path: path + ".schedule", Message: "Cron 表达式无效"})
		}
		if _, err := time.LoadLocation(item.Timezone); err != nil {
			issues = append(issues, ImportIssue{Path: path + ".timezone", Message: "时区无效"})
		}
		if item.TimeoutSeconds < 5 || item.TimeoutSeconds > 3600 {
			issues = append(issues, ImportIssue{Path: path + ".timeoutSeconds", Message: "超时必须在 5-3600 秒之间"})
		}
		if item.RetryTimes < 0 || item.RetryTimes > 10 || (item.RetryInterval != 0 && (item.RetryInterval < 1 || item.RetryInterval > 3600)) {
			issues = append(issues, ImportIssue{Path: path + ".retry", Message: "重试参数无效"})
		}
		task := Task{RoomID: roomID, Action: item.Action, WorldIDs: item.WorldIDs, Parameters: item.Parameters}
		if err := validateAction(task); err != nil {
			issues = append(issues, ImportIssue{Path: path + ".action", Message: err.Error()})
		}
	}
	return issues
}
