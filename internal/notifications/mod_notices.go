package notifications

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"dont/internal/dstruntime"
	"github.com/jinzhu/gorm"
)

type modNoticeRecord struct {
	ID        string `gorm:"primary_key;type:char(64)"`
	Window    int64
	Sources   string    `gorm:"type:text"`
	UpdatedAt time.Time `gorm:"index"`
}

func validNoticeToken(value string, max int) bool {
	if len(value) == 0 || len(value) > max {
		return false
	}
	for _, c := range value {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func (s *Service) ObserveModNotice(room, world, producer string, e dstruntime.RuntimeEvent) error {
	if e.Kind != "mod.notice" || !dstruntime.ValidModNotice(e.Fields) || room == "" || world == "" || !validNoticeToken(producer, 128) || e.Sequence < 1 {
		return nil
	}
	at := time.Unix(e.OccurredAtUnix, 0).UTC()
	if age := time.Since(at); age < -30*time.Second || age > 2*time.Minute {
		return nil
	}
	return s.project(func(p *Service) {
		id := fmt.Sprintf("mod-notice:%s:%s:%d", worldNoticeKey(room, world), producer, e.Sequence)
		hash := sha256.Sum256([]byte("mod.notice\x00" + id))
		var count int
		if err := p.store.db.Table(p.store.channelsTable+"_event").Where("event_key = ?", hex.EncodeToString(hash[:])).Count(&count).Error; err != nil {
			p.projectionError(err)
			return
		}
		if count > 0 {
			return
		}
		key := worldNoticeKey(room, world)
		table := p.store.channelsTable + "_mod_notice"
		var row modNoticeRecord
		err := p.store.db.Table(table).Where("id = ?", key).First(&row).Error
		if err != nil && !gorm.IsRecordNotFoundError(err) {
			p.projectionError(err)
			return
		}
		window := time.Now().Unix() / 60
		counts := map[string]int{}
		if row.Window == window && row.Sources != "" {
			if err = json.Unmarshal([]byte(row.Sources), &counts); err != nil {
				p.projectionError(err)
				return
			}
		}
		source := e.Fields["source"].(string)
		code := e.Fields["code"].(string)
		total := 0
		for _, v := range counts {
			total += v
		}
		if total >= 20 || counts[source] >= 5 || counts[source] == 0 && len(counts) >= 8 {
			return
		}
		counts[source]++
		data, err := json.Marshal(counts)
		if err != nil {
			p.projectionError(err)
			return
		}
		row = modNoticeRecord{ID: key, Window: window, Sources: string(data), UpdatedAt: time.Now().UTC()}
		if err = p.store.db.Table(table).Save(&row).Error; err != nil {
			p.projectionError(err)
			return
		}
		p.fact(Event{ID: id, Kind: "mod.notice", Category: "mods", RoomID: room, Title: "模组消息", Message: noticeText(e.Fields["message"].(string), 1024), Status: "observed", Severity: "info", OccurredAt: at, Facts: &Facts{WorldID: world, Source: source, Code: code}})
	})
}
