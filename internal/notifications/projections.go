package notifications

import (
	"crypto/sha256"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"dont/internal/console"
	"dont/internal/dstruntime"
	"dont/internal/gameupdate"
	"dont/internal/jobs"
	"dont/internal/modupdates"
	"dont/internal/runtimeaudit"
	"dont/internal/softwareupdate"
	"dont/internal/worldstate"
	"github.com/jinzhu/gorm"
)

type worldNoticeRecord struct {
	ID         string `gorm:"primary_key;type:char(64)"`
	RoomID     string `gorm:"type:varchar(255);index"`
	WorldID    string `gorm:"type:varchar(255)"`
	WorldName  string
	SessionID  string
	Season     string
	Day        *int
	ObservedAt time.Time
}

type changeNoticeRecord struct {
	ID        string `gorm:"primary_key;type:varchar(160)"`
	RunID     string `gorm:"type:varchar(64);index"`
	RoomID    string `gorm:"type:varchar(255);index"`
	WorldID   string
	Action    string
	BeforeDay *int
	Status    string
	LoadedAt  *time.Time
	CreatedAt time.Time `gorm:"index"`
}

type operationNoticeRecord struct {
	ID         string `gorm:"primary_key;type:varchar(255)"`
	RoomID     string `gorm:"type:varchar(255);index"`
	Status     string
	WorldScope string    `gorm:"type:text"`
	UpdatedAt  time.Time `gorm:"index"`
}

func worldNoticeKey(room, world string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(room+"\x00"+world)))
}
func (s *Service) fact(event Event) {
	event.SchemaVersion = 1
	if s.projectionTx {
		if event.OccurredAt.IsZero() {
			event.OccurredAt = time.Now().UTC()
		}
		s.projected = append(s.projected, event)
	} else {
		s.project(func(p *Service) { p.fact(event) })
	}
}

// ObserveWorld reuses current telemetry reads; it never starts a game poller.
func (s *Service) ObserveWorld(value worldstate.Snapshot) {
	if !s.projectionTx {
		s.project(func(p *Service) { p.ObserveWorld(value) })
		return
	}
	if value.Stale || value.Cycles == nil || value.ObservedAt.IsZero() || value.SessionID == "" {
		return
	}
	table := s.store.channelsTable + "_world"
	key := worldNoticeKey(value.RoomID, value.WorldID)
	var previous worldNoticeRecord
	err := s.store.db.Table(table).Where("id = ?", key).First(&previous).Error
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		s.projectionError(err)
		return
	}
	if !value.ObservedAt.After(previous.ObservedAt) {
		return
	}
	day := *value.Cycles + 1
	next := worldNoticeRecord{ID: key, RoomID: value.RoomID, WorldID: value.WorldID, WorldName: value.WorldName, SessionID: value.SessionID, Season: value.Season, Day: &day, ObservedAt: value.ObservedAt}
	if err = s.store.db.Table(table).Save(&next).Error; err != nil {
		s.projectionError(err)
		return
	}
	var pending []changeNoticeRecord
	if err = s.store.db.Table(s.store.channelsTable+"_change").Where("room_id = ? AND world_id = ? AND status = ? AND created_at >= ?", value.RoomID, value.WorldID, "waiting", time.Now().UTC().Add(-10*time.Minute)).Limit(8).Find(&pending).Error; err != nil {
		s.projectionError(err)
		return
	}
	for _, change := range pending {
		if change.LoadedAt == nil || value.ObservedAt.Before(*change.LoadedAt) {
			continue
		}
		label := "回档"
		if change.Action == "regenerate" {
			label = "重置"
		}
		if change.Action == "restore" {
			label = "恢复备份"
		}
		message := fmt.Sprintf("%s指令后，%s已重新运行，当前第 %d 天。目标存档或新地图结果仍待确认。", label, value.WorldName, day)
		if change.BeforeDay != nil {
			message = fmt.Sprintf("%s指令后，%s已重新运行：第 %d 天 → 第 %d 天。目标存档或新地图结果仍待确认。", label, value.WorldName, *change.BeforeDay, day)
		}
		s.fact(Event{ID: change.ID + ":observed", Kind: "world.change", Category: "maintenance", RoomID: value.RoomID, Status: "uncertain", Severity: "warning", Title: label + "后世界状态", Message: message, OccurredAt: value.ObservedAt, Facts: &Facts{Action: change.Action, OperationID: change.RunID, WorldID: value.WorldID, WorldName: value.WorldName, BeforeDay: change.BeforeDay, Day: &day, Season: value.Season}})
		if err := s.store.db.Table(s.store.channelsTable+"_change").Where("id = ?", change.ID).UpdateColumn("status", "observed").Error; err != nil {
			s.projectionError(err)
			return
		}
	}
	// Initial samples, session changes and maintenance establish a baseline.
	if previous.Season == "" || previous.SessionID != value.SessionID || previous.Season == value.Season || len(pending) > 0 || !strings.EqualFold(value.WorldRole, "master") {
		return
	}
	seasons := map[string]string{"spring": "春季", "summer": "夏季", "autumn": "秋季", "winter": "冬季"}
	season, known := seasons[value.Season]
	if !known {
		return
	}
	message := fmt.Sprintf("进入%s，当前第 %d 天。", season, day)
	if value.RemainingDaysInSeason != nil {
		message += fmt.Sprintf("本季剩余 %d 天。", *value.RemainingDaysInSeason)
	}
	s.fact(Event{ID: fmt.Sprintf("season:%s:%d", key, value.ObservedAt.UnixNano()), Kind: "world.season", Category: "season", RoomID: value.RoomID, Status: "observed", Severity: "info", Title: "季节变化", Message: message, OccurredAt: value.ObservedAt, Facts: &Facts{WorldID: value.WorldID, Day: &day, Season: value.Season, RemainingDays: value.RemainingDaysInSeason}})
}

// ObserveCommand only accepts runs created by the fixed server templates. A
// synchronous console receipt does not confirm an asynchronous map operation.
func (s *Service) ObserveCommand(run console.Run) {
	if !s.projectionTx {
		s.project(func(p *Service) { p.ObserveCommand(run) })
		return
	}
	if run.Mode != "builtin" || (run.CommandID != "rollback" && run.CommandID != "regenerate") {
		return
	}
	label := "回档"
	if run.CommandID == "regenerate" {
		label = "重置世界"
	}
	if run.Status == console.RunSending {
		var worlds []worldNoticeRecord
		q := s.store.db.Table(s.store.channelsTable+"_world").Where("room_id = ? AND world_id = ?", run.RoomID, run.WorldID)
		if err := q.Limit(32).Find(&worlds).Error; err != nil {
			s.projectionError(err)
			return
		}
		if len(worlds) == 0 {
			worlds = []worldNoticeRecord{{WorldID: run.WorldID}}
		}
		for _, world := range worlds {
			before := world.Day
			if world.ObservedAt.After(run.CreatedAt) || run.CreatedAt.Sub(world.ObservedAt) > 2*time.Minute {
				before = nil
			}
			record := changeNoticeRecord{ID: run.ID + ":" + worldNoticeKey(run.RoomID, world.WorldID), RunID: run.ID, RoomID: run.RoomID, WorldID: world.WorldID, Action: run.CommandID, BeforeDay: before, Status: "waiting", CreatedAt: run.CreatedAt}
			if err := s.store.db.Table(s.store.channelsTable+"_change").Where("id = ?", record.ID).FirstOrCreate(&record).Error; err != nil {
				s.projectionError(err)
			}
		}
		message := "已开始" + label + "，等待游戏重新加载后确认结果。"
		if run.CommandID == "rollback" {
			if points, ok := run.Arguments["days"]; ok {
				message = fmt.Sprintf("准备回退 %v 个存档点；存档点数不等于游戏天数。", points)
			}
		}
		s.fact(Event{ID: "command:" + run.ID + ":requested", Kind: "world.change", Category: "maintenance", RoomID: run.RoomID, Status: "running", Severity: "info", Title: label + "请求", Message: message, OccurredAt: run.CreatedAt, Facts: &Facts{Action: run.CommandID, OperationID: run.ID, WorldID: run.WorldID}})
		return
	}
	if run.Status == console.RunSending || run.Status == console.RunSent {
		return
	}
	status, message, severity := "uncertain", "指令已提交，等待重新加载后的实际状态；尚未确认"+label+"完成。", "warning"
	if run.Status == console.RunFailed && !run.MayHaveExecuted {
		status, message, severity = "failed", label+"指令执行失败。", "error"
		if err := s.store.db.Table(s.store.channelsTable+"_change").Where("run_id = ?", run.ID).UpdateColumn("status", "failed").Error; err != nil {
			s.projectionError(err)
			return
		}
	}
	s.fact(Event{ID: "command:" + run.ID + ":" + status, Kind: "world.change", Category: "maintenance", RoomID: run.RoomID, Status: status, Severity: severity, Title: label + "状态", Message: message, Facts: &Facts{Action: run.CommandID, OperationID: run.ID, WorldID: run.WorldID}})
}

func (s *Service) ObserveRuntime(value runtimeaudit.Event) {
	if !s.projectionTx {
		s.project(func(p *Service) { p.ObserveRuntime(value) })
		return
	}
	if value.Type == runtimeaudit.EventRunning {
		if err := s.store.db.Table(s.store.channelsTable+"_change").Where("room_id = ? AND world_id = ? AND status = ? AND created_at < ?", value.RoomID, value.WorldID, "waiting", value.OccurredAt.UTC()).UpdateColumn("loaded_at", value.OccurredAt.UTC()).Error; err != nil {
			s.projectionError(err)
			return
		}
	}
	if value.Type != runtimeaudit.EventRunning && value.Type != runtimeaudit.EventStopped && value.Type != runtimeaudit.EventUnexpectedExit && value.Type != runtimeaudit.EventFailed {
		return
	}
	var active int
	if err := s.store.db.Table(s.store.channelsTable+"_operation").Where("room_id = ? AND status = ? AND updated_at >= ? AND (world_scope = ? OR instr(world_scope, ?) > 0)", value.RoomID, "running", time.Now().UTC().Add(-time.Hour), "", "\x1f"+value.WorldID+"\x1f").Count(&active).Error; err != nil {
		s.projectionError(err)
		return
	}
	var changing int
	if err := s.store.db.Table(s.store.channelsTable+"_change").Where("room_id = ? AND world_id = ? AND status = ? AND created_at >= ?", value.RoomID, value.WorldID, "waiting", time.Now().UTC().Add(-10*time.Minute)).Count(&changing).Error; err != nil {
		s.projectionError(err)
		return
	}
	if (active > 0 || changing > 0) && value.Type != runtimeaudit.EventUnexpectedExit && value.Type != runtimeaudit.EventFailed {
		return
	}
	title, message, severity := "世界已启动", "世界已完成加载。", "info"
	if value.Type == runtimeaudit.EventStopped {
		title, message = "世界已停止", "世界已停止运行。"
	}
	if value.Type == runtimeaudit.EventUnexpectedExit {
		title, message, severity = "世界异常停止", "检测到世界退出，未找到对应的正常停止记录。", "error"
	}
	if value.Type == runtimeaudit.EventFailed {
		title, message, severity = "世界启动失败", "世界未能正常启动，请查看面板中的运行记录。", "error"
	}
	s.fact(Event{ID: fmt.Sprintf("runtime:%d", value.ID), Kind: "room.operation", Category: "maintenance", RoomID: value.RoomID, JobID: value.JobID, Status: string(value.Type), Severity: severity, Title: title, Message: message, OccurredAt: value.OccurredAt, Facts: &Facts{WorldID: value.WorldID, OperationID: value.OperationID}})
}

func (s *Service) ObserveJob(job jobs.Job) {
	if job.Kind == "agent.software.update" || job.Kind == "agent.upgrade" {
		if job.Status != jobs.StatusSucceeded && job.Status != jobs.StatusFailed {
			return
		}
		for _, target := range job.Targets {
			message, severity := "节点管理程序已更新并重新连接。", "info"
			if target.Status != jobs.StatusSucceeded {
				message, severity = "节点管理程序更新未确认完成，请查看任务详情。", "warning"
			}
			s.fact(Event{ID: "agent-result:" + job.ID + ":" + target.TargetID, Kind: "software.result", Category: "software", TargetID: target.TargetID, Status: string(target.Status), Severity: severity, Title: "节点程序更新", Message: message, JobID: job.ID})
		}
		return
	}
	if strings.HasPrefix(job.Kind, "rooms.") && job.RoomID == "" {
		grouped := map[string][]jobs.Target{}
		for _, target := range job.Targets {
			parts := strings.SplitN(target.TargetID, ":", 2)
			if len(parts) == 2 {
				target.TargetID = parts[1]
				grouped[parts[0]] = append(grouped[parts[0]], target)
			}
		}
		for room, targets := range grouped {
			scoped := job
			scoped.Kind = "room." + strings.TrimPrefix(job.Kind, "rooms.")
			scoped.RoomID = room
			scoped.Targets = targets
			if job.Status != jobs.StatusRunning {
				scoped.Status = jobs.StatusSucceeded
				scoped.Outcome = jobs.OutcomeFull
				ok := 0
				for _, t := range targets {
					if t.Status == jobs.StatusSucceeded {
						ok++
					} else {
						scoped.Status = jobs.StatusFailed
					}
				}
				if ok > 0 && ok < len(targets) {
					scoped.Outcome = jobs.OutcomePartial
				}
			}
			s.ObserveJob(scoped)
		}
		return
	}
	if job.RoomID != "" && (job.Kind == "mod.update" || job.Kind == "mod.download") && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed) {
		message, severity := "模组文件已更新；运行中的相关世界需要重启后才能加载新版，已停止的世界下次启动时加载。", "info"
		if job.Status == jobs.StatusFailed {
			message, severity = "模组文件更新未完成，请查看任务详情。", "warning"
		}
		s.fact(Event{ID: "mod-files:" + job.ID, Kind: "mod.activation", Category: "updates", RoomID: job.RoomID, JobID: job.ID, Status: map[bool]string{true: "prepared", false: "failed"}[job.Status == jobs.StatusSucceeded], Severity: severity, Title: "模组文件更新", Message: message, Facts: &Facts{Action: "mod.update", RequiresRestart: job.Status == jobs.StatusSucceeded}})
		return
	}
	if !s.projectionTx {
		s.project(func(p *Service) { p.ObserveJob(job) })
		return
	}
	if job.RoomID == "" {
		return
	}
	action := strings.TrimPrefix(job.Kind, "room.")
	labels := map[string]string{"start": "启动", "stop": "停止", "restart": "重启", "save": "保存"}
	label, ok := labels[action]
	category, kind := "maintenance", "room.operation"
	if !ok {
		if strings.HasPrefix(job.Kind, "backup") || job.Kind == "automation.backup.create" {
			label, category, kind = "备份", "backup", "backup.result"
			if strings.Contains(job.Kind, "restore") || strings.Contains(job.Kind, "recover") {
				label, category = "恢复备份", "maintenance"
			}
		} else {
			return
		}
	}
	if label == "恢复备份" && job.Status == jobs.StatusRunning {
		var worlds []worldNoticeRecord
		if err := s.store.db.Table(s.store.channelsTable+"_world").Where("room_id = ?", job.RoomID).Limit(32).Find(&worlds).Error; err != nil {
			s.projectionError(err)
			return
		}
		for _, world := range worlds {
			before := world.Day
			if time.Since(world.ObservedAt) > 2*time.Minute {
				before = nil
			}
			record := changeNoticeRecord{ID: job.ID + ":" + worldNoticeKey(job.RoomID, world.WorldID), RunID: job.ID, RoomID: job.RoomID, WorldID: world.WorldID, Action: "restore", BeforeDay: before, Status: "waiting", CreatedAt: time.Now().UTC()}
			if err := s.store.db.Table(s.store.channelsTable+"_change").Where("id = ?", record.ID).FirstOrCreate(&record).Error; err != nil {
				s.projectionError(err)
				return
			}
		}
	}
	status := string(job.Status)
	if job.Status != jobs.StatusRunning && job.Status != jobs.StatusSucceeded && job.Status != jobs.StatusFailed && job.Status != jobs.StatusCanceled {
		return
	}
	if category == "maintenance" {
		record := operationNoticeRecord{ID: job.ID + ":" + job.RoomID, RoomID: job.RoomID, Status: status, UpdatedAt: time.Now().UTC()}
		if strings.HasPrefix(job.Kind, "room.") {
			for _, target := range job.Targets {
				record.WorldScope += "\x1f" + target.TargetID + "\x1f"
			}
		}
		if err := s.store.db.Table(s.store.channelsTable + "_operation").Save(&record).Error; err != nil {
			s.projectionError(err)
		}
	}
	message, severity := label+"正在执行。", "info"
	verified := 0
	for _, target := range job.Targets {
		if target.Status == jobs.StatusSucceeded {
			verified++
		}
	}
	switch job.Status {
	case jobs.StatusSucceeded:
		message = label + "已完成。"
		if category == "backup" {
			message = "备份任务已完成，文件是否可恢复以面板校验结果为准。"
		}
		if strings.Contains(job.Kind, "restore") || strings.Contains(job.Kind, "recover") {
			message = "备份恢复任务已完成；世界是否运行及当前天数以最新采集为准。"
		}
	case jobs.StatusFailed:
		message, severity = label+"未完成，请查看面板中的任务详情。", "error"
		if job.Outcome == jobs.OutcomePartial {
			status, message = "partial", fmt.Sprintf("%s部分完成，成功 %d/%d 个目标；请查看未完成的世界。", label, verified, len(job.Targets))
		}
	case jobs.StatusCanceled:
		message = label + "任务已取消，已经执行的步骤以当前世界状态为准。"
	}
	worldTotal, worldVerified := 0, 0
	if strings.HasPrefix(job.Kind, "room.") {
		worldTotal, worldVerified = len(job.Targets), verified
	}
	occurred := job.CreatedAt
	if job.StartedAt != nil {
		occurred = *job.StartedAt
	}
	if job.Status != jobs.StatusRunning && job.FinishedAt != nil {
		occurred = *job.FinishedAt
	}
	s.fact(Event{OccurredAt: occurred, ID: "lifecycle:" + job.ID + ":" + job.RoomID + ":" + status, Kind: kind, Category: category, RoomID: job.RoomID, JobID: job.ID, Status: status, Severity: severity, Title: label + "状态", Message: message, Facts: &Facts{Action: action, OperationID: job.ID, WorldID: job.WorldID, VerifiedWorlds: worldVerified, TotalWorlds: worldTotal}})
}

func (s *Service) ObserveMods(value modupdates.State) {
	if !s.projectionTx {
		s.project(func(p *Service) { p.ObserveMods(value) })
		return
	}
	kind, title, message, status := "mod.activation", "模组更新", "", string(value.Status)
	switch value.Status {
	case modupdates.StatusAvailable:
		if len(value.AvailableModIDs) == 0 {
			return
		}
		kind, title, message = "mod.available", "发现模组新版", fmt.Sprintf("%d 个模组有更新；运行中的世界需要重启才能加载新版。", len(value.AvailableModIDs))
	case modupdates.StatusPrepared, modupdates.StatusWaitingForPlayers:
		message = "模组文件已准备，等待运行中的世界重启加载；原已停止的世界下次启动时加载。"
	case modupdates.StatusActivating:
		message = "正在重启相关世界以加载新版模组。"
	case modupdates.StatusLoaded:
		message = "相关世界已完成重启和加载确认；模组运行情况仍以游戏实际表现为准。"
	case modupdates.StatusBlocked:
		message = "模组更新暂未完成，请查看面板中的更新详情。"
	default:
		return
	}
	ids := append([]string(nil), value.AvailableModIDs...)
	sort.Strings(ids)
	if len(ids) > 100 {
		ids = ids[:100]
	}
	identity := strings.Join(ids, ",")
	if value.Status == modupdates.StatusAvailable {
		versions := []string{}
		if value.CheckSummary != nil {
			for _, item := range value.CheckSummary.Items {
				if item.Status == "outdated" && (item.LatestRevision != "" || item.LatestVersion != "") {
					revision := item.LatestRevision
					if revision == "" {
						revision = item.LatestVersion
					}
					versions = append(versions, item.ModID+":"+revision)
				}
			}
		}
		if len(versions) == 0 {
			return
		} // Unknown remote identity is not a new-version fact.
		sort.Strings(versions)
		identity += strings.Join(versions, ",")
	}
	if value.PreparedAt != nil {
		identity += value.PreparedAt.UTC().Format(time.RFC3339Nano)
	} else if value.LastCheckedAt != nil && value.Status != modupdates.StatusAvailable {
		identity += value.LastCheckedAt.UTC().Format(time.RFC3339Nano)
	}
	if value.Status == modupdates.StatusBlocked {
		identity += value.ErrorCode
	}
	severity := "info"
	if value.Status == modupdates.StatusBlocked {
		severity = "warning"
	}
	s.fact(Event{ID: fmt.Sprintf("mods:%s:%s:%x", value.RoomID, status, sha256.Sum256([]byte(identity))), Kind: kind, Category: "updates", RoomID: value.RoomID, Status: status, Severity: severity, Title: title, Message: message, OccurredAt: value.UpdatedAt, Facts: &Facts{Action: "mod.update", ModIDs: ids, RequiresRestart: value.Status != modupdates.StatusLoaded}})
}

func (s *Service) ObserveRelease(value gameupdate.Release) {
	if !s.projectionTx {
		s.project(func(p *Service) { p.ObserveRelease(value) })
		return
	}
	stage := string(value.Stage)

	rooms := map[string][]gameupdate.ReleaseShardResult{}
	for _, world := range value.Shards {
		rooms[world.RoomID] = append(rooms[world.RoomID], world)
	}
	for room, worlds := range rooms {
		active := "running"
		if releaseTerminal(value.Stage) {
			active = "finished"
		}
		record := operationNoticeRecord{ID: "release:" + value.ID + ":" + room, RoomID: room, Status: active, UpdatedAt: value.UpdatedAt}
		for _, world := range worlds {
			record.WorldScope += "\x1f" + world.WorldID + "\x1f"
		}
		if err := s.store.db.Table(s.store.channelsTable + "_operation").Save(&record).Error; err != nil {
			s.projectionError(err)
			return
		}
		message, severity := "游戏更新正在执行。", "info"
		ready, total := 0, 0
		for _, world := range worlds {
			if world.WasRunning {
				total++
				if world.LoadConfirmedAt != nil {
					ready++
				}
			}
		}
		switch stage {
		case "notifying":
			delay := 0
			if value.Plan.Policy.StopDelaySeconds != nil {
				delay = *value.Plan.Policy.StopDelaySeconds
			}
			message = fmt.Sprintf("游戏更新即将开始，将停止相关世界；等待时间 %d 秒。", delay)
		case "updating":
			message = "正在下载并校验游戏文件，完成后恢复原先运行的世界。"
		case "succeeded":
			message = fmt.Sprintf("游戏安装已更新并校验；本房间原运行世界已恢复 %d/%d。", ready, total)
		case "failed", "recovery_required":
			message, severity = fmt.Sprintf("游戏更新未全部完成；本房间原运行世界已确认恢复 %d/%d。请查看面板处理剩余步骤。", ready, total), "warning"
		case "abandoned":
			message = "本次游戏更新维护已结束，世界保持当前状态；未继续自动启动。"
		}
		if stage != "notifying" && stage != "updating" && !releaseTerminal(value.Stage) {
			continue
		}
		s.fact(Event{ID: "release:" + value.ID + ":" + room + ":" + stage, Kind: "game.result", Category: "updates", RoomID: room, JobID: value.SourceJobID, Status: stage, Severity: severity, Title: "游戏更新", Message: message, OccurredAt: value.UpdatedAt, Facts: &Facts{Action: "game.update", OperationID: value.ID, VerifiedWorlds: ready, TotalWorlds: total}})
	}
}

func (s *Service) projectionError(err error) { s.projectionErr = err }

// Readers also expire waiting changes when telemetry has stopped. All rooms
// share one check per minute; no readers means no timer or background polling.
func (s *Service) expirePendingChanges() {
	s.factsMu.Lock()
	if time.Now().Before(s.nextExpiryCheck) {
		s.factsMu.Unlock()
		return
	}
	s.nextExpiryCheck = time.Now().Add(time.Minute)
	s.factsMu.Unlock()
	s.project(func(*Service) {})
}

// Baseline changes and their public facts commit together. A failed write can
// be retried by the next source observation without losing or duplicating facts.
func (s *Service) project(action func(*Service)) error {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	var events []Event
	prune := time.Now().After(s.nextFactPrune)
	err := s.store.db.Transaction(func(tx *gorm.DB) error {
		p := &Service{store: &Store{db: tx, channelsTable: s.store.channelsTable, deliveriesTable: s.store.deliveriesTable}, projectionTx: true}
		var expired []changeNoticeRecord
		if err := tx.Table(s.store.channelsTable+"_change").Where("status = ? AND created_at < ?", "waiting", time.Now().UTC().Add(-10*time.Minute)).Limit(32).Find(&expired).Error; err != nil {
			return err
		}
		for _, change := range expired {
			if err := tx.Table(s.store.channelsTable+"_change").Where("id = ?", change.ID).UpdateColumn("status", "expired").Error; err != nil {
				return err
			}
			p.fact(Event{ID: change.ID + ":expired", Kind: "world.change", Category: "maintenance", RoomID: change.RoomID, Status: "timeout", Severity: "warning", Title: "世界变化尚未确认", Message: "未在等待时间内读到重新加载后的状态，结果仍未确认；系统不会自动重发游戏指令。", Facts: &Facts{Action: change.Action, OperationID: change.RunID, WorldID: change.WorldID}})
		}
		action(p)
		if p.projectionErr != nil {
			return p.projectionErr
		}
		for _, event := range p.projected {
			if err := p.store.appendEventTx(tx, event); err != nil {
				return err
			}
		}
		events = p.projected
		if prune {
			return p.store.pruneFacts(tx)
		}
		return nil
	})
	if err != nil {
		s.projectionFailures.Add(1)
		log.Printf("[Notifications] project event: %v", err)
		return err
	}
	if prune {
		s.nextFactPrune = time.Now().UTC().Add(time.Hour)
	}
	if len(events) > 0 {
		s.broker.Publish()
	}
	for _, event := range events {
		if _, err := s.emitRecorded(event, "", false, true); err != nil {
			log.Printf("[Notifications] queue recorded event: %v", err)
		}
	}
	return nil
}

// ObserveSeason consumes the existing demand-driven runtime stream. Initial
// baselines, reset windows and gaps are filtered by the source before this hook.
func (s *Service) ObserveSeason(room, world, producer string, event dstruntime.RuntimeEvent) {
	if !s.projectionTx {
		s.project(func(p *Service) { p.ObserveSeason(room, world, producer, event) })
		return
	}
	if event.Kind != "world.season" {
		return
	}
	at := time.Unix(event.OccurredAtUnix, 0).UTC()
	if age := time.Since(at); age < -30*time.Second || age > 2*time.Minute {
		return
	}
	season, _ := event.Fields["season"].(string)
	name := map[string]string{"spring": "春季", "summer": "夏季", "autumn": "秋季", "winter": "冬季"}[season]
	if name == "" {
		return
	}
	var pending int
	if err := s.store.db.Table(s.store.channelsTable+"_change").Where("room_id = ? AND status = ? AND created_at >= ?", room, "waiting", time.Now().UTC().Add(-10*time.Minute)).Count(&pending).Error; err != nil {
		s.projectionError(err)
		return
	}
	if pending > 0 {
		return
	}
	var previous worldNoticeRecord
	err := s.store.db.Table(s.store.channelsTable+"_world").Where("id = ?", worldNoticeKey(room, world)).First(&previous).Error
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		s.projectionError(err)
		return
	}
	if err == nil && (!at.After(previous.ObservedAt) || previous.Season == season) {
		return
	}
	if err == nil {
		if err := s.store.db.Table(s.store.channelsTable+"_world").Where("id = ?", previous.ID).Updates(map[string]interface{}{"season": season, "observed_at": at}).Error; err != nil {
			s.projectionError(err)
			return
		}
	}
	s.fact(Event{ID: fmt.Sprintf("season:%s:%s:%d", worldNoticeKey(room, world), producer, event.Sequence), Kind: "world.season", Category: "season", RoomID: room, Status: "observed", Severity: "info", Title: "季节变化", Message: "季节已变为" + name + "。", OccurredAt: at, Facts: &Facts{WorldID: world, Season: season}})
}

func (s *Service) ObserveSoftware(component, target string, current string, op softwareupdate.Operation, healthy bool) {
	status, message, severity := op.Phase, "", "info"
	switch op.Phase {
	case "succeeded":
		if !healthy || softwareupdate.CompareVersions(current, op.Version) != 0 {
			return
		}
		message = "管理程序已更新至 " + current + "，并已恢复服务。游戏世界是否重启以房间记录为准。"
	case "failed", "interrupted", "rolled_back":
		message, severity = "管理程序更新未完成，请查看面板的软件更新记录。", "warning"
	case "canceled":
		message = "管理程序更新已取消。"
	default:
		return
	}
	s.fact(Event{ID: "software-result:" + component + ":" + target + ":" + op.ID + ":" + status, Kind: "software.result", Category: "software", TargetID: target, Version: op.Version, Status: status, Severity: severity, Title: "管理程序更新", Message: message, OccurredAt: op.UpdatedAt, Release: &ReleaseFacts{Component: component, CurrentVersion: current, LatestVersion: op.Version}})
}

func releaseTerminal(stage gameupdate.ReleaseStage) bool {
	switch stage {
	case "succeeded", "failed", "recovery_required", "abandoned":
		return true
	}
	return false
}

func (s *Store) pruneFacts(tx *gorm.DB) error {
	for _, item := range []struct {
		suffix, clock string
		model         interface{}
	}{
		{"_world", "observed_at", &worldNoticeRecord{}}, {"_change", "created_at", &changeNoticeRecord{}}, {"_operation", "updated_at", &operationNoticeRecord{}}, {"_performance", "updated_at", &performanceNoticeRecord{}}, {"_mod_notice", "updated_at", &modNoticeRecord{}},
	} {
		var expired []string
		if err := tx.Table(s.channelsTable+item.suffix).Where(item.clock+" < ?", time.Now().UTC().Add(-30*24*time.Hour)).Order(item.clock+" ASC").Limit(128).Pluck("id", &expired).Error; err != nil {
			return err
		}
		if len(expired) > 0 {
			if err := tx.Table(s.channelsTable+item.suffix).Where("id IN (?)", expired).Delete(item.model).Error; err != nil {
				return err
			}
		}
		expired = nil
		if err := tx.Table(s.channelsTable+item.suffix).Order(item.clock+" DESC, id DESC").Offset(feedLimit).Limit(128).Pluck("id", &expired).Error; err != nil {
			return err
		}
		if len(expired) > 0 {
			if err := tx.Table(s.channelsTable+item.suffix).Where("id IN (?)", expired).Delete(item.model).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// A successful task without a verified archive is not a verified backup.
func (s *Service) ObserveBackup(job jobs.Job, verified bool, restorable bool, worlds int) {
	status, severity, message := "uncertain", "warning", "备份任务已结束，但尚未确认文件校验结果，请查看备份详情。"
	if verified {
		status, severity, message = "succeeded", "info", "备份已生成并通过文件校验。"
		if restorable && worlds > 0 {
			message = fmt.Sprintf("完整房间备份已通过校验，包含 %d 个世界。", worlds)
		} else {
			message += "是否包含可恢复的完整存档，请查看备份详情。"
		}
	}
	at := time.Now().UTC()
	if job.FinishedAt != nil {
		at = *job.FinishedAt
	}
	s.fact(Event{ID: "lifecycle:" + job.ID + ":" + job.RoomID + ":" + status, RoomID: job.RoomID, JobID: job.ID, Kind: "backup.result", Category: "backup", Status: status, Severity: severity, Title: "备份结果", Message: message, OccurredAt: at, Facts: &Facts{Action: "backup.create", OperationID: job.ID}})
}
