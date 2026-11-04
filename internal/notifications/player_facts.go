package notifications

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"dont/internal/playeractivity"
)

// Only confirmed final forms are included. A kill records the credited player,
// not all participants or the end of a multi-boss encounter.
var noticeBosses = map[string]string{
	"deerclops": "独眼巨鹿", "bearger": "熊獾", "dragonfly": "龙蝇", "moose": "麋鹿鹅", "antlion": "蚁狮", "beequeen": "蜂王",
	"toadstool": "毒菌蟾蜍", "toadstool_dark": "悲惨的毒菌蟾蜍", "minotaur": "远古守护者", "stalker_atrium": "远古织影者",
	"crabking": "帝王蟹", "malbatross": "邪天翁", "alterguardian_phase3": "天体英雄", "daywalker": "梦魇疯猪", "daywalker2": "拾荒疯猪",
	"eyeofterror": "恐怖之眼", "twinofterror1": "激光眼", "twinofterror2": "魔焰眼", "mutateddeerclops": "晶体独眼巨鹿", "mutatedbearger": "装甲熊獾", "mutatedwarg": "附身座狼",
}

func ActivityNoticeEligible(e playeractivity.Event) bool {
	if e.Kind == "player.death" || e.Kind == "player.respawned" {
		return true
	}
	prefab, _ := e.Fields["targetPrefab"].(string)
	guid, _ := e.Fields["targetGuid"].(string)
	return e.Kind == "combat.kill" && noticeBosses[prefab] != "" && guid != ""
}

// Public notice text is plain text. Drop control characters and neutralize CQ
// segments even if a local mod or player supplied them. Never parse markup.
func noticeText(value string, limit int) string {
	if !utf8.ValidString(value) {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = strings.NewReplacer("[", "［", "]", "］").Replace(value)
	chars := []rune(value)
	if len(chars) > limit {
		value = string(chars[:limit])
	}
	return strings.TrimSpace(value)
}

func (s *Service) ObserveActivity(events []playeractivity.Event) error {
	return s.project(func(p *Service) {
		for _, e := range events {
			if !ActivityNoticeEligible(e) || e.ID == "" || e.RoomID == "" || e.WorldID == "" || e.UserID == "" || e.SessionID == "" || e.ProducerInstanceID == "" {
				continue
			}
			at := e.OccurredAt
			if age := time.Since(at); at.IsZero() || age < -30*time.Second || age > 24*time.Hour {
				continue
			}
			name := noticeText(e.Name, 128)
			if name == "" {
				name = "一位玩家"
			}
			title, message, category, kind := "玩家死亡", name+"已死亡。", "players", "player.death"
			id := "activity:" + e.ID
			facts := &Facts{WorldID: e.WorldID, PlayerID: e.UserID, PlayerName: name}
			switch e.Kind {
			case "player.death":
				if prefab, _ := e.Fields["targetPrefab"].(string); noticeBosses[prefab] != "" {
					message = name + "被" + noticeBosses[prefab] + "击败。"
					facts.EntityPrefab = prefab
				}
			case "player.respawned":
				title, message, kind = "玩家复活", name+"已从幽灵状态复活。", "player.respawned"
			case "combat.kill":
				prefab, _ := e.Fields["targetPrefab"].(string)
				guid, _ := e.Fields["targetGuid"].(string)
				if !validNoticeToken(guid, 32) {
					continue
				}
				// Multiple credited kills of the same entity share one identity. Runtime
				// instance/session prevent GUID reuse after restarts from swallowing kills.
				id = fmt.Sprintf("boss:%s:%s:%s", worldNoticeKey(e.RoomID, e.WorldID), worldNoticeKey(e.SessionID, e.ProducerInstanceID), guid)
				title, message, category, kind = "Boss 击杀", name+"击杀了"+noticeBosses[prefab]+"。", "boss", "boss.killed"
				facts.EntityPrefab = prefab
			}
			p.fact(Event{ID: id, Kind: kind, Category: category, RoomID: e.RoomID, Title: title, Message: message, Status: "observed", Severity: "info", OccurredAt: at, Facts: facts})
		}
	})
}
