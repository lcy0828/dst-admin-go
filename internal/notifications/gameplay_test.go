package notifications

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/performance"
	"dont/internal/playeractivity"
)

func playerFact(id, kind string) playeractivity.Event {
	return playeractivity.Event{ID: id, Kind: kind, RoomID: "example", WorldID: "caves", SessionID: "session", ProducerInstanceID: "producer", UserID: "KU_EXAMPLE", Name: "示例玩家", OccurredAt: time.Now().UTC(), Fields: playeractivity.EventFields{}}
}
func TestPlayerFactsRespectEvidenceAndBossIdentity(t *testing.T) {
	s, _, _ := fixture(t)
	death := playerFact("death", "player.death")
	death.Name = "测试[CQ:at,qq=all]\n玩家"
	respawn := playerFact("respawn", "player.respawned")
	kill := playerFact("kill", "combat.kill")
	kill.Fields = playeractivity.EventFields{"targetPrefab": "dragonfly", "targetGuid": "12345"}
	duplicate := kill
	duplicate.ID = "another-credit"
	duplicate.UserID = "KU_OTHER"
	nonboss := playerFact("spider", "combat.kill")
	nonboss.Fields = playeractivity.EventFields{"targetPrefab": "spider", "targetGuid": "12346", "epic": true}
	middle := playerFact("middle", "combat.kill")
	middle.Fields = playeractivity.EventFields{"targetPrefab": "alterguardian_phase2", "targetGuid": "12347", "epic": true}
	noGUID := playerFact("missing", "combat.kill")
	noGUID.Fields = playeractivity.EventFields{"targetPrefab": "dragonfly"}
	unknown := playerFact("unknown", "player.joined")
	if err := s.ObserveActivity([]playeractivity.Event{death, respawn, kill, duplicate, nonboss, middle, noGUID, unknown}); err != nil {
		t.Fatal(err)
	}
	items := roomNotices(t, s, "example")
	if len(items) != 3 {
		t.Fatalf("expected death, respawn and one boss kill: %+v", items)
	}
	if strings.Contains(items[0].Message, "[CQ:") || strings.Contains(items[0].Message, "被") {
		t.Fatal("unsafe text or invented death reason", items[0])
	}
	if items[2].Kind != "boss.killed" || items[2].Facts.EntityPrefab != "dragonfly" || !strings.Contains(items[2].Message, "龙蝇") {
		t.Fatal(items[2])
	}
	if err := s.ObserveActivity([]playeractivity.Event{death, respawn, kill}); err != nil {
		t.Fatal(err)
	}
	if len(roomNotices(t, s, "example")) != 3 {
		t.Fatal("replay duplicated events")
	}
	kill.ID = "new-kill"
	kill.ProducerInstanceID = "new-instance"
	s.ObserveActivity([]playeractivity.Event{kill})
	if len(roomNotices(t, s, "example")) != 4 {
		t.Fatal("valid new instance swallowed GUID reuse")
	}
	for _, e := range []playeractivity.Event{nonboss, middle, noGUID, unknown} {
		if ActivityNoticeEligible(e) {
			t.Fatal("unconfirmed gameplay notification admitted", e)
		}
	}
}
func TestActivityProjectionFailureIsRetryable(t *testing.T) {
	s, store, _ := fixture(t)
	store.db.Exec("CREATE TRIGGER fail_gameplay BEFORE INSERT ON test_notification_channel_event BEGIN SELECT RAISE(FAIL, 'synthetic failure'); END")
	e := playerFact("death", "player.death")
	if s.ObserveActivity([]playeractivity.Event{e}) == nil {
		t.Fatal("failed projection was acknowledged")
	}
	store.db.Exec("DROP TRIGGER fail_gameplay")
	if err := s.ObserveActivity([]playeractivity.Event{e}); err != nil {
		t.Fatal(err)
	}
	if len(roomNotices(t, s, "example")) != 1 {
		t.Fatal("retry lost or duplicated event")
	}
}

func performanceFact(at time.Time, score float64) performance.Point {
	return performance.Point{RoomID: "example", WorldID: "master", WorldName: "Master", InstanceID: "instance", At: at, State: "running", Score: &score, WindowSeconds: 30, ValidSeconds: 30, ScoreVersion: performance.ScoreVersion}
}
func TestPerformanceAlertSustainedRecoveryRestartAndCooldown(t *testing.T) {
	s, store, _ := fixture(t)
	start := time.Now().Add(-9 * time.Minute).UTC()
	s.watchPerformance("example")
	for i := 0; i < 2; i++ {
		s.ObservePerformance(performanceFact(start.Add(time.Duration(i)*30*time.Second), 50))
	}
	if len(roomNotices(t, s, "example")) != 0 {
		t.Fatal("transient score alerted")
	}
	s.ObservePerformance(performanceFact(start.Add(time.Minute), 50))
	if items := roomNotices(t, s, "example"); len(items) != 1 || items[0].Status != "slow" {
		t.Fatal(items)
	}
	// Persistent active state prevents another alert after a Controller restart.
	rebuilt, err := New(store, &testMail{})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt.watchPerformance("example")
	for i := 3; i < 6; i++ {
		rebuilt.ObservePerformance(performanceFact(start.Add(time.Duration(i)*30*time.Second), 45))
	}
	if len(roomNotices(t, rebuilt, "example")) != 1 {
		t.Fatal("restart repeated active alarm")
	}
	for i := 6; i < 9; i++ {
		rebuilt.ObservePerformance(performanceFact(start.Add(time.Duration(i)*30*time.Second), 90))
	}
	if items := roomNotices(t, rebuilt, "example"); len(items) != 2 || items[1].Status != "recovered" {
		t.Fatal(items)
	}
	for i := 9; i < 12; i++ {
		rebuilt.ObservePerformance(performanceFact(start.Add(time.Duration(i)*30*time.Second), 40))
	}
	if len(roomNotices(t, rebuilt, "example")) != 2 {
		t.Fatal("cooldown ignored")
	}
}
func TestPerformanceMissingPauseAndUnsubscribedNeverBecomeZero(t *testing.T) {
	s, store, _ := fixture(t)
	now := time.Now().Add(-5 * time.Minute)
	for i := 0; i < 6; i++ {
		s.ObservePerformance(performanceFact(now.Add(time.Duration(i)*30*time.Second), 20))
	}
	var count int
	store.db.Table(store.channelsTable + "_performance").Count(&count)
	if count != 0 {
		t.Fatal("unsubscribed observer wrote state")
	}
	for _, state := range []string{"paused", "idle", "stopped", "warming"} {
		t.Run(state, func(t *testing.T) {
			observer, _, _ := fixture(t)
			observer.watchPerformance("example")
			for i := 0; i < 2; i++ {
				observer.ObservePerformance(performanceFact(now.Add(time.Duration(i)*30*time.Second), 20))
			}
			invalid := performanceFact(now.Add(time.Minute), 0)
			invalid.State = state
			invalid.Score = nil
			invalid.ValidSeconds = 0
			observer.ObservePerformance(invalid)
			observer.ObservePerformance(performanceFact(now.Add(90*time.Second), 20))
			if len(roomNotices(t, observer, "example")) != 0 {
				t.Fatal("pause or unavailable samples caused alert")
			}
		})
	}
}
func TestPerformanceInstanceChangeAndProjectionFailureDoNotProduceFalseRecovery(t *testing.T) {
	s, store, _ := fixture(t)
	s.watchPerformance("example")
	at := time.Now().Add(-3 * time.Minute)
	for i := 0; i < 2; i++ {
		s.ObservePerformance(performanceFact(at.Add(time.Duration(i)*30*time.Second), 50))
	}
	if err := store.db.Exec("CREATE TRIGGER fail_alert BEFORE INSERT ON test_notification_channel_event BEGIN SELECT RAISE(FAIL, 'synthetic failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	point := performanceFact(at.Add(time.Minute), 50)
	s.ObservePerformance(point)
	var row performanceNoticeRecord
	store.db.Table(store.channelsTable + "_performance").First(&row)
	if row.Active || row.LowSamples != 2 {
		t.Fatal("state advanced without notification", row)
	}
	store.db.Exec("DROP TRIGGER fail_alert")
	s.ObservePerformance(point)
	for i := 3; i < 6; i++ {
		v := performanceFact(at.Add(time.Duration(i)*30*time.Second), 95)
		v.InstanceID = "new-instance"
		s.ObservePerformance(v)
	}
	if len(roomNotices(t, s, "example")) != 1 {
		t.Fatal("restart claimed old instance recovered")
	}
}
func TestPerformanceFiltersDoNotStartGameChecks(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.RoomFeed("example", "", "runtime.alert", 10, true); err != nil {
		t.Fatal(err)
	}
	if !s.performanceWanted("example") || len(s.watchedGames()) != 0 {
		t.Fatal("category leases are mixed")
	}
	s.mu.Lock()
	s.alertWatches["example"] = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if s.performanceWanted("example") {
		t.Fatal("expired demand kept working")
	}
}

func modFact(seq int64, source, text string) dstruntime.RuntimeEvent {
	return dstruntime.RuntimeEvent{Sequence: seq, Kind: "mod.notice", OccurredAtUnix: time.Now().Unix(), Fields: map[string]any{"source": source, "code": "level_up", "message": text}}
}
func TestModNoticesBoundedDeduplicatedAndPlainText(t *testing.T) {
	s, store, _ := fixture(t)
	for i := int64(1); i <= 9; i++ {
		e := modFact(i, "example_mod", "示例玩家 [CQ:at,qq=all] 技能升级")
		if err := s.ObserveModNotice("example", "caves", "producer", e); err != nil {
			t.Fatal(err)
		}
		s.ObserveModNotice("example", "caves", "producer", e)
	}
	if items := roomNotices(t, s, "example"); len(items) != 5 || strings.Contains(items[0].Message, "[CQ:") || items[0].Facts.WorldID != "caves" {
		t.Fatal(items)
	}
	rebuilt, err := New(store, &testMail{})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt.ObserveModNotice("example", "caves", "producer", modFact(10, "example_mod", "仍在限流"))
	if len(roomNotices(t, rebuilt, "example")) != 5 {
		t.Fatal("restart bypassed quota")
	}
	for i := int64(11); i < 80; i++ {
		rebuilt.ObserveModNotice("example", "caves", "producer", modFact(i, fmt.Sprintf("source_%d", i/5), "测试消息"))
	}
	if len(roomNotices(t, rebuilt, "example")) > 20 {
		t.Fatal("global per-world quota bypassed")
	}
	bad := modFact(100, "test_mod", "hello")
	bad.Fields["category"] = "maintenance"
	rebuilt.ObserveModNotice("example", "master", "producer", bad)
	if items := roomNotices(t, rebuilt, "example"); len(items) > 20 {
		t.Fatal(items)
	}
}

func TestCollectionPolicyIsReadOnlyCachedAndDoesNotLeakAcrossRooms(t *testing.T) {
	s, _, _ := fixture(t)
	calls := 0
	s.ConfigureCollectionStatus(func(room string) map[string]string {
		calls++
		if room == "example" {
			return map[string]string{"players": "disabled", "boss": "disabled", "alerts": "enabled", "private": "secret"}
		}
		return map[string]string{"players": "enabled"}
	})
	page, err := s.RoomFeed("example", "", "player.death", 10, true)
	if err != nil || page.Collection["players"] != "disabled" || page.Collection["private"] != "" {
		t.Fatal(page, err)
	}
	if len(page.SupportedKinds) != len(EventKinds) {
		t.Fatal("supported kinds omitted")
	}
	page.Collection["players"] = "spoofed"
	next, err := s.RoomFeed("example", "", "player.death", 10, true)
	if err != nil || calls != 1 || next.Collection["players"] != "disabled" {
		t.Fatal("cache mutated or unbounded reads", next, err, calls)
	}
	other, _ := s.RoomFeed("other", "", "player.death", 10, true)
	if other.Collection["players"] != "enabled" || calls != 2 {
		t.Fatal("cross-room policy cache")
	}
	if len(s.watchedGames()) != 0 || s.performanceWanted("example") {
		t.Fatal("policy read enabled collectors")
	}
}
