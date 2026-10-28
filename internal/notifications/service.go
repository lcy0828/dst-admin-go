package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/jobs"
	"github.com/google/uuid"
)

type MailSender interface {
	SendMail(context.Context, []string, string, string) error
}

type Service struct {
	store           *Store
	mail            MailSender
	mu              sync.RWMutex
	editMu          sync.Mutex
	channels        []Channel
	agents          map[string]string
	channelsChanged func()
	wake            chan struct{}
}

func New(store *Store, mail MailSender) (*Service, error) {
	if store == nil || mail == nil {
		return nil, errors.New("notification store and mail sender are required")
	}
	s := &Service{store: store, mail: mail, wake: make(chan struct{}, 1), agents: map[string]string{}}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) reload() error {
	items, err := s.store.Channels()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.channels = items
	s.agents = map[string]string{}
	changed := s.channelsChanged
	s.mu.Unlock()
	if changed != nil {
		changed()
	}
	s.notify()
	return nil
}

func (s *Service) Channels() ([]Channel, error) { return s.store.Channels() }
func (s *Service) History(limit, offset int) ([]Delivery, int, error) {
	return s.store.History(limit, offset)
}
func (s *Service) Save(id string, input ChannelInput) (Channel, error) {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	channel, err := s.store.Save(id, input)
	if err != nil {
		return Channel{}, err
	}
	return channel, s.reload()
}
func (s *Service) Delete(id, revision string) error {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if err := s.store.Delete(id, revision); err != nil {
		return err
	}
	return s.reload()
}

// Reestablish an Agent status baseline when event subscriptions change. This
// uses the Controller's cached inventory and never polls remote Agents.
func (s *Service) SetChannelChangeListener(listener func()) {
	s.mu.Lock()
	s.channelsChanged = listener
	s.mu.Unlock()
}
func (s *Service) Retry(id string) error {
	if err := s.store.Retry(id); err != nil {
		return err
	}
	s.notify()
	return nil
}

func (s *Service) Active(kind string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, channel := range s.channels {
		if channel.Enabled && slices.Contains(channel.Events, kind) {
			return true
		}
	}
	return false
}

func (s *Service) emit(event Event, channelID string, test bool) ([]Delivery, error) {
	s.mu.RLock()
	var selected []Channel
	for _, channel := range s.channels {
		if test && channel.ID == channelID || !test && channel.Enabled && slices.Contains(channel.Events, event.Kind) && (!channel.FailuresOnly || event.Kind == "software.available" || event.Kind == "agent.software.available" || event.Severity == "error" || event.Severity == "warning") {
			selected = append(selected, channel)
		}
	}
	s.mu.RUnlock()
	if test && len(selected) == 0 {
		return nil, ErrNotFound
	}
	if len(selected) == 0 {
		return []Delivery{}, nil
	}
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	items, err := s.store.Enqueue(event, selected)
	if err == nil {
		s.notify()
	}
	return items, err
}

func (s *Service) Emit(event Event) {
	if _, err := s.emit(event, "", false); err != nil {
		log.Printf("[Notifications] queue event: %v", err)
	}
}

func (s *Service) Test(channelID string) ([]Delivery, error) {
	return s.emit(Event{Kind: "custom", Title: "DST Admin 测试通知 / Test notification", Message: "这是一条测试通知，邮件或 Webhook 已接入 DST Admin。\nThis is a test notification from DST Admin.", Status: "test", Severity: "info"}, channelID, true)
}

func (s *Service) Send(title, message string) ([]Delivery, error) {
	if strings.TrimSpace(title) == "" || len(title) > 200 || strings.ContainsAny(title, "\r\n\x00") || strings.TrimSpace(message) == "" || len(message) > 8192 || strings.ContainsRune(message, '\x00') {
		return nil, ErrInvalid
	}
	return s.emit(Event{Kind: "custom", Title: title, Message: message, Status: "custom", Severity: "info"}, "", false)
}

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Run(ctx context.Context) {
	if err := s.store.Recover(); err != nil {
		log.Printf("[Notifications] recover deliveries: %v", err)
	}
	// One bounded worker sleeps indefinitely when there is no pending delivery.
	for ctx.Err() == nil {
		item, err := s.store.Next()
		if errors.Is(err, ErrNotFound) {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		if err != nil {
			if !s.wait(ctx, 5*time.Second) {
				return
			}
			continue
		}
		if delay := time.Until(item.NextAttemptAt); delay > 0 {
			if !s.wait(ctx, delay) {
				return
			}
			continue
		}
		began, err := s.store.Begin(item.ID)
		if err != nil || !began {
			if !s.wait(ctx, time.Second) {
				return
			}
			continue
		}
		item.Attempts++
		channel, exists := s.channel(item.ChannelID)
		result := SendResult{}
		canceled := !exists || !channel.Enabled && item.Event.Status != "test"
		if !canceled {
			sendCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			result = s.deliver(sendCtx, channel, item.Event)
			cancel()
		}
		if ctx.Err() != nil {
			result = SendResult{ErrorCode: "DELIVERY_INTERRUPTED"}
		}
		if err := s.store.Finish(item, result, canceled); err != nil {
			log.Printf("[Notifications] save delivery result: %v", err)
		}
		if err := s.store.Prune(); err != nil {
			log.Printf("[Notifications] prune delivery history: %v", err)
		}
	}
}

func (s *Service) wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	case <-timer.C:
		return true
	}
}

func (s *Service) channel(id string) (Channel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, channel := range s.channels {
		if channel.ID == id {
			return channel, true
		}
	}
	return Channel{}, false
}

func (s *Service) OnJob(job jobs.Job) {
	if job.Status != jobs.StatusSucceeded && job.Status != jobs.StatusFailed {
		return
	}
	// Successful periodic checks are observations, not applied updates.
	if job.Status == jobs.StatusSucceeded && job.Kind == "mod.update.check" {
		return
	}
	kind, title := "", ""
	switch {
	case strings.HasPrefix(job.Kind, "room."), strings.HasPrefix(job.Kind, "rooms."):
		kind, title = "room.result", "房间操作 / Room operation"
	case strings.HasPrefix(job.Kind, "backup"), job.Kind == "automation.backup.create":
		kind, title = "backup.result", "存档备份与恢复 / Save backup and restore"
	case strings.HasPrefix(job.Kind, "game."), strings.HasPrefix(job.Kind, "game-installation."):
		kind, title = "game.result", "游戏安装与更新 / Game installation and update"
	case strings.HasPrefix(job.Kind, "mod."):
		kind, title = "mod.result", "模组操作 / Mod operation"
	}
	if kind == "" || !s.Active(kind) {
		return
	}
	severity, status := "info", "成功 / Succeeded"
	if job.Status == jobs.StatusFailed {
		severity, status = "error", "失败 / Failed"
	}
	message := "操作 / Operation: " + job.Kind + "\n结果 / Result: " + status
	if job.RoomID != "" {
		message += "\n房间 / Room: " + job.RoomID
	}
	if job.WorldID != "" {
		message += "\n世界 / World: " + job.WorldID
	}
	// Raw command output and task error text may contain credentials. Link the
	// task identity instead of forwarding arbitrary logs to external endpoints.
	if job.Error != nil {
		message += "\n错误代码 / Error code: " + job.Error.Code
	}
	// Only forward bounded, allowlisted domain states, never arbitrary details.
	for _, target := range job.Targets {
		if target.Error == nil || target.Error.Details == nil {
			continue
		}
		details := target.Error.Details
		if code, ok := details["failureCode"].(string); ok {
			switch code {
			case "SAVE_IN_PROGRESS", "TARGET_SNAPSHOT_PASSED", "SAVE_WRITE_FAILED", "BARRIER_EXPIRED", "BACKUP_TIMEOUT", "SNAPSHOT_ROUND_UNCONFIRMED", "RUNTIME_UPDATE_REQUIRED", "HOT_BACKUP_UNAVAILABLE", "TOPOLOGY_CHANGED", "ARCHIVE_INTEGRITY_FAILED", "CONTROLLER_RESTARTED", "BACKUP_FAILED":
				message += "\n备份原因 / Backup cause: " + code
			}
		}
		if state, ok := details["artifactState"].(string); ok {
			switch state {
			case "not_generated", "staging", "available", "partial", "missing", "corrupt":
				message += "\n备份文件 / Backup artifacts: " + state
			}
		}
		if phase, ok := details["failurePhase"].(string); ok {
			switch phase {
			case "planned", "barrier_preparing", "barrier_arming", "barrier_committing", "barrier_waiting", "barrier_retrying", "staging", "transferring":
				message += "\n失败阶段 / Failed phase: " + phase
			}
		}
		if state, ok := details["barrierReleaseState"].(string); ok {
			switch state {
			case "pending", "released", "unconfirmed":
				message += "\n保存保护 / Save protection: " + state
			}
		}
		count := func(key string) (int, bool) {
			encoded, err := json.Marshal(details[key])
			if err != nil {
				return 0, false
			}
			value, err := strconv.Atoi(string(encoded))
			return value, err == nil && value >= 0 && value <= 1000000
		}
		verified, verifiedOK := count("verifiedWorlds")
		total, totalOK := count("totalWorlds")
		if verifiedOK && totalOK && verified <= total {
			message += "\n世界校验 / Verified worlds: " + strconv.Itoa(verified) + "/" + strconv.Itoa(total)
		}
		if encoded, err := json.Marshal(details["barrierAttempts"]); err == nil && len(encoded) == 1 && encoded[0] >= '0' && encoded[0] <= '3' {
			message += "\n保存尝试 / Save attempts: " + string(encoded)
		}
		break
	}
	message += "\n任务 / Task: " + job.ID
	s.Emit(Event{ID: "job:" + job.ID, Kind: kind, Title: title + " · " + status, Message: message, Status: string(job.Status), Severity: severity, RoomID: job.RoomID, JobID: job.ID})
}

func (s *Service) ObserveAgent(id, name, status string) {
	s.mu.Lock()
	previous, known := s.agents[id]
	s.agents[id] = status
	s.mu.Unlock()
	if !known || previous == status || !s.Active("agent.status") {
		return
	}
	title, severity := "Agent 已连接 / Agent connected", "info"
	if status == "offline" {
		title, severity = "Agent 已离线 / Agent offline", "error"
	}
	s.Emit(Event{Kind: "agent.status", Title: title, Message: name + "\n" + title, Status: status, Severity: severity, TargetID: "agent:" + id})
}
