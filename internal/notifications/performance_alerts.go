package notifications

import (
	"fmt"
	"math"
	"time"

	"dont/internal/performance"
	"github.com/jinzhu/gorm"
)

type performanceNoticeRecord struct {
	ID           string `gorm:"primary_key;type:char(64)"`
	RoomID       string `gorm:"type:varchar(255);index"`
	WorldID      string
	InstanceID   string
	Active       bool
	LastAt       time.Time
	LowSeconds   float64
	GoodSeconds  float64
	LowSamples   int
	GoodSamples  int
	LastNoticeAt time.Time
	UpdatedAt    time.Time `gorm:"index"`
}

func (s *Service) watchPerformance(room string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alertWatches == nil {
		s.alertWatches = map[string]time.Time{}
	}
	now := time.Now()
	for room, until := range s.alertWatches {
		if !now.Before(until) {
			delete(s.alertWatches, room)
		}
	}
	if len(s.alertWatches) < 128 || !s.alertWatches[room].IsZero() {
		s.alertWatches[room] = now.Add(2 * time.Minute)
	}
}
func (s *Service) performanceWanted(room string) bool {
	if s.Active("runtime.alert") {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Now().Before(s.alertWatches[room])
}

// The observer receives existing persisted samples. No extra process probes,
// timers or simulation reads are introduced by notification subscriptions.
func (s *Service) ObservePerformance(v performance.Point) {
	if v.RoomID == "" || v.WorldID == "" || v.At.IsZero() || !s.performanceWanted(v.RoomID) {
		return
	}
	if age := time.Since(v.At); age < -30*time.Second || age > 10*time.Minute {
		return
	}
	s.project(func(p *Service) {
		table := p.store.channelsTable + "_performance"
		key := worldNoticeKey(v.RoomID, v.WorldID)
		var row performanceNoticeRecord
		err := p.store.db.Table(table).Where("id = ?", key).First(&row).Error
		if err != nil && !gorm.IsRecordNotFoundError(err) {
			p.projectionError(err)
			return
		}
		if !v.At.After(row.LastAt) {
			return
		}
		gap := v.At.Sub(row.LastAt).Seconds()
		if row.InstanceID != v.InstanceID {
			row = performanceNoticeRecord{ID: key, RoomID: v.RoomID, WorldID: v.WorldID, InstanceID: v.InstanceID}
		}
		// Missing, paused, idle and warming-up samples break continuity. CPU and
		// memory usage alone never imply game simulation slowdown.
		valid := v.ScoreVersion == performance.ScoreVersion && v.State == "running" && v.Reason == "" && v.InstanceID != "" && v.Score != nil && !math.IsNaN(*v.Score) && !math.IsInf(*v.Score, 0) && *v.Score >= 0 && *v.Score <= 100 && v.ValidSeconds > 0 && v.ValidSeconds <= v.WindowSeconds && v.WindowSeconds <= 900
		if row.LastAt.IsZero() || gap <= 0 || math.Abs(gap-v.WindowSeconds) > 5 || !valid {
			row.LowSeconds, row.GoodSeconds, row.LowSamples, row.GoodSamples = 0, 0, 0, 0
		}
		if valid {
			switch {
			case *v.Score <= 60:
				row.LowSeconds += v.ValidSeconds
				row.LowSamples++
				row.GoodSeconds, row.GoodSamples = 0, 0
			case *v.Score >= 80:
				row.GoodSeconds += v.ValidSeconds
				row.GoodSamples++
				row.LowSeconds, row.LowSamples = 0, 0
			default:
				row.LowSeconds, row.GoodSeconds, row.LowSamples, row.GoodSamples = 0, 0, 0, 0
			}
			status, title, message, severity := "", "", "", "warning"
			if !row.Active && row.LowSeconds >= 90 && row.LowSamples >= 3 && (row.LastNoticeAt.IsZero() || v.At.Sub(row.LastNoticeAt) >= 10*time.Minute) {
				row.Active = true
				status, title = "slow", "游戏持续变慢"
				message = fmt.Sprintf("%s连续多次运行评分偏低，最新 %.0f/100；请在面板查看性能报表。", noticeWorldName(v.WorldName), *v.Score)
			} else if row.Active && row.GoodSeconds >= 90 && row.GoodSamples >= 3 {
				row.Active = false
				status, title, severity = "recovered", "游戏运行已恢复", "info"
				message = fmt.Sprintf("%s连续多次运行评分恢复，最新 %.0f/100。", noticeWorldName(v.WorldName), *v.Score)
			}
			if status != "" {
				row.LastNoticeAt = v.At
				p.fact(Event{ID: fmt.Sprintf("performance:%s:%d:%s", key, v.At.UnixNano(), status), Kind: "runtime.alert", Category: "alerts", RoomID: v.RoomID, Title: title, Message: message, Status: status, Severity: severity, OccurredAt: v.At, Facts: &Facts{WorldID: v.WorldID, WorldName: v.WorldName, Score: v.Score}})
			}
		}
		row.LastAt = v.At
		row.UpdatedAt = time.Now().UTC()
		// Saturate counters; a long-running server never grows unbounded state.
		row.LowSeconds = math.Min(row.LowSeconds, 3600)
		row.GoodSeconds = math.Min(row.GoodSeconds, 3600)
		row.LowSamples = min(row.LowSamples, 256)
		row.GoodSamples = min(row.GoodSamples, 256)
		p.projectionError(p.store.db.Table(table).Save(&row).Error)
	})
}
func noticeWorldName(name string) string {
	switch name {
	case "Master":
		return "地面"
	case "Caves":
		return "洞穴"
	}
	if v := noticeText(name, 80); v != "" {
		return v
	}
	return "世界"
}
