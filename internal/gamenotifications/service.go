package gamenotifications

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/shared"
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type Runtime interface {
	Status(context.Context, string, string) (shared.ShardRuntimeStatus, error)
	SendID(context.Context, string, string, shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error)
}

type OnlineCounter interface {
	OnlinePlayers(context.Context, string) (int, error)
}

type OnlineCounterFunc func(context.Context, string) (int, error)

func (f OnlineCounterFunc) OnlinePlayers(ctx context.Context, roomID string) (int, error) {
	return f(ctx, roomID)
}

type SendPlan struct {
	Notification Notification
	Targets      []jobs.TargetSpec
	Runner       func(string) jobs.Runner
}

type Service struct {
	rooms   RoomCatalog
	runtime Runtime
	store   *Store
	online  OnlineCounter
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
}

func NewService(roomCatalog RoomCatalog, runtime Runtime, store *Store) (*Service, error) {
	if roomCatalog == nil || runtime == nil || store == nil {
		return nil, errors.New("rooms, runtime, and game notification store are required")
	}
	return &Service{rooms: roomCatalog, runtime: runtime, store: store, now: time.Now, wait: waitContext}, nil
}

func (s *Service) ConfigureOnlineCounter(counter OnlineCounter) {
	s.online = counter
}

type MaintenancePreview struct {
	Policy        Policy    `json:"policy"`
	OnlinePlayers *int      `json:"onlinePlayers"`
	CheckedAt     time.Time `json:"checkedAt"`
	Warning       string    `json:"warning,omitempty"`
}

func (s *Service) PreviewMaintenance(ctx context.Context, roomID string) (MaintenancePreview, error) {
	policy, err := s.Policy(roomID)
	if err != nil {
		return MaintenancePreview{}, err
	}
	value := MaintenancePreview{Policy: policy}
	if s.online != nil {
		count, countErr := s.online.OnlinePlayers(ctx, roomID)
		if countErr == nil {
			value.OnlinePlayers = &count
		} else {
			log.Printf("[GameNotification] preview online players room=%s: %v", roomID, countErr)
		}
	}
	value.CheckedAt = s.now().UTC()
	if value.OnlinePlayers == nil {
		value.Warning = "暂时无法确认在线人数，不会按空服处理"
	}
	return value, nil
}

func (s *Service) Prepare(roomID, message string, source Source) (SendPlan, error) {
	room, worlds, message, err := s.validateSend(roomID, message, source)
	if err != nil {
		return SendPlan{}, err
	}
	notification, err := s.store.Create(room, message, source, worlds)
	if err != nil {
		return SendPlan{}, err
	}
	targets := make([]jobs.TargetSpec, 0, len(worlds))
	for _, world := range worlds {
		targets = append(targets, jobs.TargetSpec{ID: world.ID, Name: world.Name})
	}
	return SendPlan{
		Notification: notification,
		Targets:      targets,
		Runner: func(jobID string) jobs.Runner {
			return func(ctx context.Context, report func(jobs.TargetResult)) error {
				if err := s.store.AttachJob(notification.ID, jobID); err != nil {
					s.MarkSubmissionFailed(notification.ID, err)
					return err
				}
				_, err := s.deliver(ctx, notification.ID, room, worlds, report)
				return err
			}
		},
	}, nil
}

func (s *Service) MarkSubmissionFailed(notificationID string, cause error) {
	message := "无法创建通知发送任务"
	if cause != nil {
		message = cause.Error()
	}
	if err := s.store.Fail(notificationID, message); err != nil {
		log.Printf("[GameNotification] mark submission failed notification=%s: %v", notificationID, err)
	}
}

func (s *Service) List(filter ListFilter) (List, error) {
	if strings.TrimSpace(filter.RoomID) != "" {
		if _, err := s.rooms.Room(filter.RoomID); err != nil {
			return List{}, err
		}
	}
	return s.store.List(filter)
}

func (s *Service) Policy(roomID string) (Policy, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return Policy{}, err
	}
	if !room.Managed {
		return Policy{}, ErrRoomUnmanaged
	}
	return s.store.Policy(room.ID)
}

func (s *Service) SavePolicy(roomID string, input PolicyInput) (Policy, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return Policy{}, err
	}
	if !room.Managed {
		return Policy{}, ErrRoomUnmanaged
	}
	fields := make(map[string]string)
	if input.CountdownSeconds < MinCountdownSeconds || input.CountdownSeconds > MaxCountdownSeconds {
		fields["countdownSeconds"] = fmt.Sprintf("倒计时必须在 %d 到 %d 秒之间", MinCountdownSeconds, MaxCountdownSeconds)
	}
	if len(fields) > 0 {
		return Policy{}, &FieldError{Fields: fields}
	}
	return s.store.SavePolicy(Policy{RoomID: room.ID, Enabled: input.Enabled, CountdownSeconds: input.CountdownSeconds})
}

// BeforeOperation implements the shard lifecycle notification hook. Delivery
// failures are retained in history but do not prevent an administrator from
// stopping a room; cancellation always stops both the countdown and action.
func (s *Service) BeforeOperation(ctx context.Context, roomID, action, trigger, jobID string) error {
	return s.BeforeOperations(ctx, []string{roomID}, action, trigger, jobID)
}

// BeforeOperations coordinates one countdown timeline for operations that
// affect multiple rooms. A room with a shorter policy joins the timeline only
// when its own configured warning window begins.
func (s *Service) BeforeOperations(ctx context.Context, roomIDs []string, action, trigger, jobID string) error {
	source, verb, valid := lifecycleMessage(action, trigger)
	if !valid {
		return nil
	}
	type roomCountdown struct {
		roomID      string
		active      bool
		checkpoints map[int]bool
	}
	seen := make(map[string]bool, len(roomIDs))
	rooms := make([]roomCountdown, 0, len(roomIDs))
	for _, roomID := range roomIDs {
		roomID = strings.TrimSpace(roomID)
		if roomID == "" || seen[roomID] {
			continue
		}
		seen[roomID] = true
		policy, err := s.Policy(roomID)
		if err != nil {
			log.Printf("[GameNotification] load policy room=%s action=%s: %v", roomID, action, err)
			continue
		}
		if !policy.Enabled {
			continue
		}
		if s.online != nil {
			online, countErr := s.online.OnlinePlayers(ctx, roomID)
			if countErr == nil && online <= 0 {
				continue
			}
			if countErr != nil {
				log.Printf("[GameNotification] count online players room=%s: %v", roomID, countErr)
			}
		}
		checkpoints := make(map[int]bool)
		for _, remaining := range countdownCheckpoints(policy.CountdownSeconds) {
			checkpoints[remaining] = true
		}
		if len(checkpoints) > 0 {
			rooms = append(rooms, roomCountdown{roomID: roomID, active: true, checkpoints: checkpoints})
		}
	}

	remainingValues := func(before int) []int {
		values := make(map[int]bool)
		for _, room := range rooms {
			if !room.active {
				continue
			}
			for remaining := range room.checkpoints {
				if before <= 0 || remaining < before {
					values[remaining] = true
				}
			}
		}
		result := make([]int, 0, len(values))
		for remaining := range values {
			result = append(result, remaining)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(result)))
		return result
	}

	remaining := 0
	if values := remainingValues(0); len(values) > 0 {
		remaining = values[0]
	}
	for remaining > 0 {
		delivered := false
		for index := range rooms {
			room := &rooms[index]
			if !room.active || !room.checkpoints[remaining] {
				continue
			}
			message := fmt.Sprintf("服务器将在 %d 秒后%s，请及时保存并前往安全位置。", remaining, verb)
			notification, sendErr := s.sendDirect(ctx, room.roomID, message, source, jobID)
			if sendErr != nil {
				if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
					return sendErr
				}
				log.Printf("[GameNotification] countdown delivery room=%s action=%s: %v", room.roomID, action, sendErr)
				continue
			}
			if notification.SuccessCount == 0 && notification.FailureCount == 0 {
				room.active = false
				continue
			}
			delivered = true
		}
		next := 0
		if values := remainingValues(remaining); len(values) > 0 {
			next = values[0]
		}
		if !delivered && next > 0 {
			remaining = next
			continue
		}
		if !delivered && next == 0 {
			return nil
		}
		if err := s.wait(ctx, time.Duration(remaining-next)*time.Second); err != nil {
			return err
		}
		remaining = next
	}
	return nil
}

func (s *Service) SendAutomation(ctx context.Context, roomID, message, jobID string) (int, int, int, error) {
	value, err := s.sendDirect(ctx, roomID, message, SourceAutomation, jobID)
	return value.SuccessCount, value.FailureCount, value.SkippedCount, err
}

func (s *Service) sendDirect(ctx context.Context, roomID, message string, source Source, jobID string) (Notification, error) {
	room, worlds, message, err := s.validateSend(roomID, message, source)
	if err != nil {
		return Notification{}, err
	}
	notification, err := s.store.Create(room, message, source, worlds)
	if err != nil {
		return Notification{}, err
	}
	if strings.TrimSpace(jobID) != "" {
		if err := s.store.AttachJob(notification.ID, jobID); err != nil {
			return Notification{}, err
		}
	}
	return s.deliver(ctx, notification.ID, room, worlds, nil)
}

func (s *Service) deliver(ctx context.Context, notificationID string, room rooms.Room, worlds []rooms.World, report func(jobs.TargetResult)) (Notification, error) {
	notification, err := s.store.Get(notificationID)
	if err != nil {
		return Notification{}, err
	}
	if err := s.store.MarkSending(notificationID); err != nil {
		return Notification{}, err
	}
	// The game forwards c_announce between shards, including across machines.
	// Submit once, preferring the Master, and record other shards as skipped.
	worlds = append([]rooms.World(nil), worlds...)
	sort.SliceStable(worlds, func(i, j int) bool { return worlds[i].IsMaster && !worlds[j].IsMaster })
	broadcastWorld := ""
	success, failure, skipped, canceled := 0, 0, 0, 0
	for index, world := range worlds {
		if err := ctx.Err(); err != nil {
			for _, pending := range worlds[index:] {
				delivery := Delivery{WorldID: pending.ID, Status: DeliveryCanceled, ErrorCode: "JOB_CANCELED", ErrorMessage: "任务已取消"}
				if recordErr := s.store.RecordDelivery(notificationID, delivery); recordErr != nil {
					return Notification{}, recordErr
				}
				canceled++
				if report != nil {
					report(jobs.TargetResult{TargetID: pending.ID, Status: jobs.StatusCanceled, Error: &jobs.Error{Code: "JOB_CANCELED", Message: "任务已取消"}})
				}
			}
			completed, completeErr := s.store.Complete(notificationID, StatusCanceled, success, failure, skipped, canceled)
			if completeErr != nil {
				return Notification{}, completeErr
			}
			return completed, err
		}
		now := s.now().UTC()
		delivery := Delivery{WorldID: world.ID, SentAt: &now}
		var targetResult jobs.TargetResult
		status, statusErr := s.runtime.Status(ctx, room.ID, world.ID)
		if statusErr != nil {
			delivery.Status, delivery.ErrorCode, delivery.ErrorMessage = DeliveryFailed, "RUNTIME_STATUS_FAILED", statusErr.Error()
			failure++
			targetResult = jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: delivery.ErrorCode, Message: delivery.ErrorMessage}}
		} else if status.State != "running" || !status.SessionExists {
			delivery.Status, delivery.Message = DeliverySkipped, "分片未运行，未发送"
			skipped++
			targetResult = jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusSucceeded, Message: delivery.Message}
		} else if broadcastWorld != "" {
			delivery.Status, delivery.Message = DeliverySkipped, "已向「"+broadcastWorld+"」提交房间广播，此分片未重复发送"
			skipped++
			targetResult = jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusSucceeded, Message: delivery.Message}
		} else {
			broadcastWorld = world.Name
			result, sendErr := s.runtime.SendID(ctx, room.ID, world.ID, shared.RuntimeConsoleRequest{
				Mode: shared.ConsoleModeManaged, Command: "c_announce(" + quoteLua(notification.Message) + ")",
			})
			delivery.TargetID, delivery.AgentID = result.TargetID, result.AgentID
			delivery.TopologyRevision = result.TopologyRevision
			if !result.ObservedAt.IsZero() {
				observed := result.ObservedAt.UTC()
				delivery.ObservedAt = &observed
			}
			if sendErr != nil {
				delivery.Status, delivery.ErrorCode, delivery.ErrorMessage = DeliveryFailed, "NOTIFICATION_SEND_FAILED", sendErr.Error()
				failure++
				targetResult = jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: delivery.ErrorCode, Message: delivery.ErrorMessage}}
			} else {
				delivery.Status, delivery.Message = DeliverySucceeded, strings.TrimSpace(result.Message)
				if delivery.Message == "" {
					delivery.Message = "游戏通知已发送到分片控制台"
				}
				success++
				targetResult = jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusSucceeded, Message: delivery.Message}
			}
		}
		if err := s.store.RecordDelivery(notificationID, delivery); err != nil {
			return Notification{}, err
		}
		if report != nil {
			report(targetResult)
		}
	}
	finalStatus := completionStatus(success, failure, skipped, canceled)
	return s.store.Complete(notificationID, finalStatus, success, failure, skipped, canceled)
}

func (s *Service) validateSend(roomID, message string, source Source) (rooms.Room, []rooms.World, string, error) {
	room, err := s.rooms.Room(strings.TrimSpace(roomID))
	if err != nil {
		return rooms.Room{}, nil, "", err
	}
	if !room.Managed {
		return rooms.Room{}, nil, "", ErrRoomUnmanaged
	}
	message = strings.TrimSpace(message)
	fields := make(map[string]string)
	if message == "" || !utf8.ValidString(message) || utf8.RuneCountInString(message) > MaxMessageRunes || strings.ContainsRune(message, '\x00') {
		fields["message"] = fmt.Sprintf("通知内容必须是 1 到 %d 个字符的有效文本", MaxMessageRunes)
	}
	if !validSource(source) {
		fields["source"] = "通知来源无效"
	}
	if len(fields) > 0 {
		return rooms.Room{}, nil, "", &FieldError{Fields: fields}
	}
	worlds, err := s.rooms.Worlds(room.ID)
	if err != nil {
		return rooms.Room{}, nil, "", err
	}
	if len(worlds) == 0 {
		return rooms.Room{}, nil, "", rooms.ErrWorldNotFound
	}
	return room, worlds, message, nil
}

func validSource(source Source) bool {
	switch source {
	case SourceManual, SourceRoomStop, SourceRoomRestart, SourceGameUpdate, SourceModSync, SourceAutomation:
		return true
	default:
		return false
	}
}

func lifecycleMessage(action, trigger string) (Source, string, bool) {
	switch strings.TrimSpace(trigger) {
	case string(SourceGameUpdate):
		if action == "restart" {
			return SourceGameUpdate, "更新并重启", true
		}
		if action == "stop" {
			return SourceGameUpdate, "停止并更新", true
		}
	case string(SourceModSync):
		if action == "restart" {
			return SourceModSync, "重启以应用模组变更", true
		}
	case string(SourceAutomation):
		if action == "stop" {
			return SourceAutomation, "停止", true
		}
		if action == "restart" {
			return SourceAutomation, "重启", true
		}
	default:
		if action == "stop" {
			return SourceRoomStop, "停止", true
		}
		if action == "restart" {
			return SourceRoomRestart, "重启", true
		}
	}
	return "", "", false
}

func completionStatus(success, failure, skipped, canceled int) Status {
	if canceled > 0 {
		return StatusCanceled
	}
	if failure > 0 && success > 0 {
		return StatusPartial
	}
	if failure > 0 {
		return StatusFailed
	}
	if success > 0 {
		return StatusSucceeded
	}
	if skipped > 0 {
		return StatusSkipped
	}
	return StatusFailed
}

func countdownCheckpoints(seconds int) []int {
	values := []int{seconds, 30, 10}
	result := make([]int, 0, len(values))
	seen := make(map[int]bool, len(values))
	for _, value := range values {
		if value <= 0 || value > seconds || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func waitContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func quoteLua(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, character := range value {
		switch character {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
