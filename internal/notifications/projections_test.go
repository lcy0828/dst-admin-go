package notifications

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"dont/internal/console"
	"dont/internal/gameupdate"
	"dont/internal/jobs"
	"dont/internal/mods"
	"dont/internal/modupdates"
	"dont/internal/runtimeaudit"
	"dont/internal/softwareupdate"
	"dont/internal/worldstate"
)

func noticeWorld(at time.Time, season string, cycles int) worldstate.Snapshot {
	return worldstate.Snapshot{RoomID: "example", WorldID: "master", WorldName: "Master", WorldRole: "master", SessionID: "example-session", Season: season, Cycles: &cycles, ObservedAt: at}
}
func roomNotices(t *testing.T, s *Service, room string) []Event {
	t.Helper()
	p, e := s.RoomFeed(room, "", "", 100, false)
	if e != nil {
		t.Fatal(e)
	}
	return p.Items
}
func TestRoomFeedIsolatesScopeSeedsTailAndSurvivesRestart(t *testing.T) {
	s, store, _ := fixture(t)
	s.fact(Event{ID: "one", RoomID: "example", Kind: "world.season", Category: "season"})
	s.fact(Event{ID: "other", RoomID: "other", Kind: "world.season", Category: "season"})
	s.fact(Event{ID: "global", Kind: "software.result", Category: "software"})
	p, e := s.RoomFeed("example", "", "", 10, true)
	if e != nil || len(p.Items) != 0 {
		t.Fatal(p, e)
	}
	if _, e = s.RoomFeed("other", p.NextCursor, "", 10, false); e != ErrInvalid {
		t.Fatal("cross-room cursor accepted")
	}
	if _, e = s.Feed(p.NextCursor, "", 10); e != ErrInvalid {
		t.Fatal("room cursor accepted globally")
	}
	s.fact(Event{ID: "two", RoomID: "example", Kind: "world.change", Category: "maintenance"})
	rebuilt, e := New(store, &testMail{})
	if e != nil {
		t.Fatal(e)
	}
	next, e := rebuilt.RoomFeed("example", p.NextCursor, "", 10, false)
	if e != nil || len(next.Items) != 1 || next.Items[0].ID != "two" {
		t.Fatal(next, e)
	}
	for _, limit := range []int{0, 101} {
		if _, e = s.RoomFeed("example", "", "", limit, false); e != ErrInvalid {
			t.Fatal("unbounded read")
		}
	}
}
func TestSeasonFactsAtomicAndRepeatInFollowingYear(t *testing.T) {
	s, store, _ := fixture(t)
	now := time.Now().UTC()
	s.ObserveWorld(noticeWorld(now, "autumn", 10))
	if len(roomNotices(t, s, "example")) != 0 {
		t.Fatal("initial baseline broadcast")
	}
	// Force event persistence failure after the new world baseline was written.
	if e := store.db.Exec("CREATE TRIGGER reject_notice BEFORE INSERT ON test_notification_channel_event BEGIN SELECT RAISE(FAIL, 'test write failure'); END").Error; e != nil {
		t.Fatal(e)
	}
	s.ObserveWorld(noticeWorld(now.Add(time.Second), "winter", 11))
	var state worldNoticeRecord
	store.db.Table(store.channelsTable + "_world").First(&state)
	if state.Season != "autumn" {
		t.Fatal("baseline advanced without event")
	}
	store.db.Exec("DROP TRIGGER reject_notice")
	for i, season := range []string{"winter", "spring", "summer", "autumn", "winter"} {
		w := noticeWorld(now.Add(time.Duration(i+1)*time.Second), season, 12+i)
		s.ObserveWorld(w)
		s.ObserveWorld(w)
	}
	items := roomNotices(t, s, "example")
	if len(items) != 5 {
		t.Fatalf("repeated year or duplicate handling: %+v", items)
	}
	s.ObserveWorld(noticeWorld(now, "summer", 10))
	if len(roomNotices(t, s, "example")) != 5 {
		t.Fatal("old telemetry changed season")
	}
}
func TestRollbackReportsObservedDayAndNeverClaimsTargetWasLoaded(t *testing.T) {
	s, _, _ := fixture(t)
	now := time.Now().UTC()
	s.ObserveWorld(noticeWorld(now.Add(-time.Second), "autumn", 76))
	run := console.Run{ID: "example-run", RoomID: "example", WorldID: "master", Mode: "builtin", CommandID: "rollback", Arguments: map[string]interface{}{"days": 2}, Status: console.RunSending, CreatedAt: now}
	s.ObserveCommand(run)
	run.Status = console.RunSucceeded
	s.ObserveCommand(run)
	s.ObserveRuntime(runtimeaudit.Event{ID: 1, RoomID: "example", WorldID: "master", Type: runtimeaudit.EventRunning, OccurredAt: now.Add(time.Second)})
	s.ObserveWorld(noticeWorld(now.Add(2*time.Second), "summer", 73))
	items := roomNotices(t, s, "example")
	last := items[len(items)-1]
	if last.Status != "uncertain" || last.Facts.Day == nil || *last.Facts.Day != 74 || last.Facts.BeforeDay == nil || *last.Facts.BeforeDay != 77 {
		t.Fatalf("invented rollback result: %+v", last)
	}
	if strings.Contains(last.Message, "75 天") {
		t.Fatal("subtracted savepoints from game day")
	}
	for _, e := range items {
		if e.Kind == "world.season" {
			t.Fatal("rollback season broadcast separately")
		}
	}
	count := len(items)
	s.ObserveWorld(noticeWorld(now.Add(3*time.Second), "summer", 73))
	if len(roomNotices(t, s, "example")) != count {
		t.Fatal("duplicate rollback observation")
	}
}

func TestRecoveryRetriesFailedProjectionAndOnlyReplaysPersistedFactsOnce(t *testing.T) {
	s, store, _ := fixture(t)
	calls := 0
	s.ConfigureRecovery(func() error {
		calls++
		s.ObserveJob(jobs.Job{ID: "persisted-job", Kind: "room.restart", RoomID: "example", Status: jobs.StatusSucceeded, CreatedAt: time.Now().UTC()})
		return nil
	})
	if err := store.db.Exec("CREATE TRIGGER reject_recovery BEFORE INSERT ON test_notification_channel_event BEGIN SELECT RAISE(FAIL, 'write unavailable'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.RoomFeed("example", "", "", 10, false); err == nil {
		t.Fatal("failed projection marked recovery complete")
	}
	store.db.Exec("DROP TRIGGER reject_recovery")
	items := roomNotices(t, s, "example")
	if len(items) != 1 || calls != 2 {
		t.Fatalf("recovery lost source: calls=%d items=%+v", calls, items)
	}
	_ = roomNotices(t, s, "example")
	if calls != 2 {
		t.Fatal("recovery rescans on every read")
	}
}

func TestRecentRoomFiltersLegacyFactsBeforePaginationAndEnforcesRetention(t *testing.T) {
	s, store, _ := fixture(t)
	s.fact(Event{ID: "wanted", Kind: "backup.result", Category: "backup", RoomID: "example"})
	for i := 0; i < 3; i++ {
		s.Emit(Event{ID: fmt.Sprint("legacy-", i), Kind: "backup.result", RoomID: "example"})
	}
	items, err := s.RecentRoom("example", 1)
	if err != nil || len(items) != 1 || items[0].ID != "wanted" {
		t.Fatalf("legacy facts hid the requested history: %+v %v", items, err)
	}
	if err := store.db.Table(store.channelsTable+"_event").UpdateColumn("created_at", time.Now().UTC().Add(-31*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	items, err = s.RecentRoom("example", 1)
	if err != nil || len(items) != 0 {
		t.Fatal("expired history replayed", items, err)
	}
}

func TestInstallationModResultIsPerRoomAndNeverClaimsActivation(t *testing.T) {
	s, _, _ := fixture(t)
	s.ObserveModFiles("job", "example", []string{"123"}, true, time.Now().UTC())
	s.ObserveModFiles("job", "example", []string{"123"}, true, time.Now().UTC())
	s.ObserveModFiles("job", "other", []string{"456"}, false, time.Now().UTC())
	a, b := roomNotices(t, s, "example"), roomNotices(t, s, "other")
	if len(a) != 1 || a[0].Status != "prepared" || !a[0].Facts.RequiresRestart || !strings.Contains(a[0].Message, "需要重启") {
		t.Fatal(a)
	}
	if len(b) != 1 || b[0].Status != "uncertain" || len(b[0].Facts.ModIDs) != 1 || b[0].Facts.ModIDs[0] != "456" {
		t.Fatal("lost uncertain result or leaked another room", b)
	}
}
func TestCustomCommandsAndUnloadedSnapshotsCannotConfirmChanges(t *testing.T) {
	s, _, _ := fixture(t)
	now := time.Now().UTC()
	for _, mode := range []string{"custom", "raw"} {
		s.ObserveCommand(console.Run{ID: mode, Mode: mode, CommandID: "rollback", RoomID: "example", WorldID: "master", Status: console.RunSending, CreatedAt: now})
	}
	s.ObserveWorld(noticeWorld(now, "autumn", 30))
	if len(roomNotices(t, s, "example")) != 0 {
		t.Fatal("custom action falsely identified")
	}
	s.ObserveCommand(console.Run{ID: "fixed", Mode: "builtin", CommandID: "regenerate", RoomID: "example", WorldID: "master", Status: console.RunSending, CreatedAt: now})
	s.ObserveWorld(noticeWorld(now.Add(time.Second), "autumn", 0))
	if len(roomNotices(t, s, "example")) != 1 {
		t.Fatal("snapshot alone confirmed reset")
	}
}
func TestBatchLifecycleCropsTargetsAndMergesRuntimeNoise(t *testing.T) {
	s, _, _ := fixture(t)
	job := jobs.Job{ID: "batch", Kind: "rooms.restart", Status: jobs.StatusRunning, Targets: []jobs.Target{{TargetID: "example:master", Status: jobs.StatusRunning}, {TargetID: "example:caves", Status: jobs.StatusRunning}, {TargetID: "other:master", Status: jobs.StatusRunning}}}
	s.ObserveJob(job)
	s.ObserveRuntime(runtimeaudit.Event{ID: 1, RoomID: "example", WorldID: "master", Type: runtimeaudit.EventStopped, OccurredAt: time.Now()})
	if len(roomNotices(t, s, "example")) != 1 {
		t.Fatal("restart stop noise not suppressed")
	}
	job.Status = jobs.StatusFailed
	job.Targets[0].Status = jobs.StatusSucceeded
	job.Targets[1].Status = jobs.StatusFailed
	job.Targets[2].Status = jobs.StatusSucceeded
	s.ObserveJob(job)
	a := roomNotices(t, s, "example")
	b := roomNotices(t, s, "other")
	if len(a) != 2 || a[1].Status != "partial" || a[1].Facts.TotalWorlds != 2 || a[1].Facts.VerifiedWorlds != 1 {
		t.Fatalf("lost partial room result: %+v", a)
	}
	if len(b) != 2 || b[1].Status != "succeeded" || b[1].Facts.TotalWorlds != 1 {
		t.Fatalf("shared task leaked result: %+v", b)
	}
}
func TestModsUseActualRemoteVersionAndDoNotConfuseDownloadWithActivation(t *testing.T) {
	s, _, _ := fixture(t)
	at := time.Now().UTC()
	checked := at
	state := modupdates.State{RoomID: "example", Status: modupdates.StatusAvailable, AvailableModIDs: []string{"123"}, LastCheckedAt: &checked, UpdatedAt: at, CheckSummary: &mods.ModUpdateCheck{Items: []mods.ModUpdateCheckItem{{ModID: "123", Status: "outdated", LatestVersion: "100"}}}}
	s.ObserveMods(state)
	s.ObserveMods(state)
	state.CheckSummary.Items[0].LatestVersion = "101"
	s.ObserveMods(state)
	items := roomNotices(t, s, "example")
	if len(items) != 2 {
		t.Fatal("new remote version lost or old version repeated", items)
	}
	state.Status = modupdates.StatusPrepared
	state.PreparedAt = &at
	s.ObserveMods(state)
	items = roomNotices(t, s, "example")
	last := items[len(items)-1]
	if last.Status != "prepared" || !last.Facts.RequiresRestart || strings.Contains(last.Message, "已生效") {
		t.Fatal("download treated as activated")
	}
}
func TestSoftwareSuccessRequiresActualHealthyVersion(t *testing.T) {
	s, _, _ := fixture(t)
	op := softwareupdate.Operation{ID: "update", Version: "v2.0.0", Phase: "succeeded", UpdatedAt: time.Now()}
	s.ObserveSoftware("controller", "", "v1.0.0", op, true)
	s.ObserveSoftware("controller", "", "v2.0.0", op, false)
	p, _ := s.Feed("", "software.result", 10)
	if len(p.Items) != 0 {
		t.Fatal("false software success")
	}
	s.ObserveSoftware("controller", "", "v2.0.0", op, true)
	s.ObserveSoftware("controller", "", "v2.0.0", op, true)
	p, _ = s.Feed("", "software.result", 10)
	if len(p.Items) != 1 || p.Items[0].RoomID != "" {
		t.Fatal("software replay or game lifecycle confusion", p)
	}
}
func TestGameNoticesUseOfficialVersionSpaceAndNoIdleProbe(t *testing.T) {
	s, _, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	called := 0
	s.RunGameChecks(ctx, func(context.Context, []string) { called++ })
	if called != 0 {
		t.Fatal("idle version work")
	}
	release := gameupdate.OfficialRelease{Version: "200", CheckedAt: time.Now()}
	base := gameupdate.InstalledVersion{RoomIDs: []string{"example"}, Installed: true, Online: true, Branch: "public", GameVersion: "199", SteamBuild: "9999999"}
	s.ObserveGameVersions(release, []gameupdate.InstalledVersion{base})
	s.ObserveGameVersions(release, []gameupdate.InstalledVersion{base})
	base.Branch = "beta"
	base.RoomIDs = []string{"beta"}
	s.ObserveGameVersions(release, []gameupdate.InstalledVersion{base})
	if len(roomNotices(t, s, "example")) != 1 || len(roomNotices(t, s, "beta")) != 0 {
		t.Fatal("version number spaces mixed")
	}
}

func TestPendingChangesExpireWithoutRetryingGameAction(t *testing.T) {
	s, store, _ := fixture(t)
	change := changeNoticeRecord{ID: "old-change", RunID: "example-run", RoomID: "example", WorldID: "master", Action: "rollback", Status: "waiting", CreatedAt: time.Now().UTC().Add(-11 * time.Minute)}
	if err := store.db.Table(store.channelsTable + "_change").Create(&change).Error; err != nil {
		t.Fatal(err)
	}
	items := roomNotices(t, s, "example")
	if len(items) != 1 || items[0].Status != "timeout" || !strings.Contains(items[0].Message, "不会自动重发") {
		t.Fatal(items)
	}
	s.ObserveWorld(noticeWorld(time.Now().UTC().Add(time.Second), "winter", 100))
	if len(roomNotices(t, s, "example")) != 1 {
		t.Fatal("repeated timeout")
	}
}

func TestSingleWorldMaintenanceDoesNotHideUnrelatedWorldState(t *testing.T) {
	s, _, _ := fixture(t)
	s.ObserveJob(jobs.Job{ID: "restart-one", Kind: "room.restart", RoomID: "example", Status: jobs.StatusRunning, Targets: []jobs.Target{{TargetID: "master"}}})
	s.ObserveRuntime(runtimeaudit.Event{ID: 1, RoomID: "example", WorldID: "master", Type: runtimeaudit.EventStopped, OccurredAt: time.Now().UTC()})
	s.ObserveRuntime(runtimeaudit.Event{ID: 2, RoomID: "example", WorldID: "caves", Type: runtimeaudit.EventStopped, OccurredAt: time.Now().UTC()})
	items := roomNotices(t, s, "example")
	if len(items) != 2 || items[1].Facts.WorldID != "caves" {
		t.Fatal("unrelated world stop was hidden", items)
	}
}
func TestBackupVerificationIsSeparateFromTaskSuccess(t *testing.T) {
	s, _, _ := fixture(t)
	job := jobs.Job{ID: "archive-one", RoomID: "example", Status: jobs.StatusSucceeded}
	s.ObserveBackup(job, false, false, 0)
	job.ID = "archive-two"
	s.ObserveBackup(job, true, true, 2)
	items := roomNotices(t, s, "example")
	if len(items) != 2 || items[0].Status != "uncertain" || items[1].Status != "succeeded" || !strings.Contains(items[1].Message, "2 个世界") {
		t.Fatal(items)
	}
}

func TestRestoreWithoutPreviousSampleReportsOnlyActualLoadedDay(t *testing.T) {
	s, _, _ := fixture(t)
	now := time.Now().UTC()
	job := jobs.Job{ID: "restore", RoomID: "example", Kind: "backup.restore", Status: jobs.StatusRunning, CreatedAt: now}
	s.TrackRestoreWorlds(job, []string{"master"})
	s.ObserveJob(job)
	s.ObserveRuntime(runtimeaudit.Event{ID: 10, RoomID: "example", WorldID: "master", Type: runtimeaudit.EventRunning, OccurredAt: now.Add(time.Second)})
	s.ObserveWorld(noticeWorld(now.Add(2*time.Second), "winter", 73))
	items := roomNotices(t, s, "example")
	last := items[len(items)-1]
	if last.Kind != "world.change" || last.Facts.Action != "restore" || last.Facts.BeforeDay != nil || last.Facts.Day == nil || *last.Facts.Day != 74 || last.Status != "uncertain" {
		t.Fatal("missing previous day invented or restore observation lost", items)
	}
}
func TestRoomFeedCanRecoverAfterDatabaseRestoreWithExplicitHistoryGap(t *testing.T) {
	s, store, _ := fixture(t)
	s.fact(Event{ID: "one", RoomID: "example", Kind: "world.season", Category: "season"})
	first, _ := s.RoomFeed("example", "", "", 10, false)
	if err := store.db.Table(store.channelsTable + "_event").Delete(&feedRecord{}).Error; err != nil {
		t.Fatal(err)
	}
	next, err := s.RoomFeed("example", first.NextCursor, "", 10, false)
	if err != nil || !next.HistoryTruncated || len(next.Items) != 0 {
		t.Fatal(next, err)
	}
}
