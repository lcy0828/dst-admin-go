package modupdates

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/mods"
	"dont/internal/players"
	"dont/internal/rooms"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type updateTestRooms struct{}

func (updateTestRooms) Room(id string) (rooms.Room, error) {
	if id != "room" {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return rooms.Room{ID: id, Name: "Room", Managed: true}, nil
}

type updateTestCatalog struct {
	checkErr          error
	updates           []string
	updateErr         error
	disabled          bool
	updateCalls       []string
	installationRooms []string
	checkStarted      chan struct{}
	checkContinue     chan struct{}
}

func (c *updateTestCatalog) CheckUpdates(context.Context, string) (mods.ActionResult, error) {
	if c.checkStarted != nil {
		close(c.checkStarted)
		<-c.checkContinue
	}
	return mods.ActionResult{ModIDs: append([]string(nil), c.updates...)}, c.checkErr
}

func (c *updateTestCatalog) UpdateRoomMods(_ context.Context, _ string, modIDs []string, _ io.Writer) (mods.ActionResult, error) {
	c.updateCalls = append(c.updateCalls, modIDs...)
	if c.updateErr != nil {
		return mods.ActionResult{}, c.updateErr
	}
	c.updates = nil // Node disks now report the downloaded versions.
	if c.disabled {
		return mods.ActionResult{}, nil
	}
	return mods.ActionResult{ModIDs: append([]string(nil), modIDs...)}, nil
}

func (c *updateTestCatalog) InstallationRoomIDs(context.Context, string, string) ([]string, error) {
	return append([]string(nil), c.installationRooms...), nil
}

type updateTestRestarter struct {
	calls int
	err   error
}

func (r *updateTestRestarter) RestartRunningWorlds(context.Context, string, string) error {
	r.calls++
	return r.err
}

type updateTestPresence struct{ values []players.PresenceSnapshot }

func (p *updateTestPresence) RefreshPresence(context.Context, string) (players.PresenceSnapshot, error) {
	if len(p.values) == 0 {
		return players.PresenceSnapshot{RoomID: "room", Fresh: true}, nil
	}
	value := p.values[0]
	if len(p.values) > 1 {
		p.values = p.values[1:]
	}
	return value, nil
}

type updateTestAnnouncer struct{ calls int }

func (a *updateTestAnnouncer) AnnounceModUpdate(context.Context, string, string, string) error {
	a.calls++
	return nil
}

type updateTestFixture struct {
	db        *gorm.DB
	service   *Service
	store     *Store
	catalog   *updateTestCatalog
	restarter *updateTestRestarter
	presence  *updateTestPresence
	now       time.Time
}

func newUpdateTestFixture(t *testing.T) *updateTestFixture {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(db, "test_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &updateTestFixture{
		db: db, store: store, catalog: &updateTestCatalog{
			updates: []string{"378160973", "1392778117"}, installationRooms: []string{"room"},
		},
		restarter: &updateTestRestarter{}, presence: &updateTestPresence{},
		now: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC),
	}
	fixture.service, err = NewService(store, updateTestRooms{}, fixture.catalog, fixture.catalog, fixture.restarter, fixture.presence, jobService)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.now = func() time.Time { return fixture.now }
	fixture.store.now = func() time.Time { return fixture.now }
	t.Cleanup(func() {
		waitForUpdateJobs(t, fixture.service.jobs)
		_ = db.Close()
	})
	return fixture
}

func TestFailedUpdateCheckRetainsKnownUpdatesAndSuccessfulCheckTime(t *testing.T) {
	for _, operation := range []string{"scheduled", "apply-now"} {
		t.Run(operation, func(t *testing.T) {
			f := newUpdateTestFixture(t)
			last := f.now.Add(-time.Hour)
			_, err := f.store.SaveState(State{RoomID: "room", Status: StatusAvailable, AvailableModIDs: []string{"100"}, LastCheckedAt: &last})
			if err != nil {
				t.Fatal(err)
			}
			f.catalog.updates = []string{"200"}
			f.catalog.checkErr = errors.New("Steam unavailable for another item")
			var state State
			if operation == "scheduled" {
				state, err = f.service.check(context.Background(), "room", "job", true)
			} else {
				state, err = f.service.applyNow(context.Background(), "room", "job")
			}
			if err == nil || state.Status != StatusBlocked || state.ErrorCode != "MOD_UPDATE_CHECK_FAILED" ||
				len(state.AvailableModIDs) != 2 || state.AvailableModIDs[0] != "100" || state.AvailableModIDs[1] != "200" ||
				state.LastCheckedAt == nil || !state.LastCheckedAt.Equal(last) {
				t.Fatalf("failed check erased evidence: state=%#v err=%v", state, err)
			}
			if len(f.catalog.updateCalls) != 0 || f.restarter.calls != 0 {
				t.Fatal("incomplete check must not download or restart")
			}
			f.catalog.checkErr, f.catalog.updates = nil, nil
			state, err = f.service.check(context.Background(), "room", "job", false)
			if err != nil || state.Status != StatusIdle || len(state.AvailableModIDs) != 0 || !state.LastCheckedAt.Equal(f.now) {
				t.Fatalf("successful current result did not clear old warning: %#v, %v", state, err)
			}
		})
	}
}

func TestNotifyPolicySupportsFiveMinuteChecksWithoutPreparing(t *testing.T) {
	f := newUpdateTestFixture(t)
	_, err := f.service.UpdatePolicy("room", PolicyInput{AutoCheck: true, CheckIntervalMinutes: 5, EmptyGraceSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.service.check(context.Background(), "room", "job", true)
	if err != nil || state.NextCheckAt == nil || !state.NextCheckAt.Equal(f.now.Add(5*time.Minute)) || state.Status != StatusAvailable {
		t.Fatalf("five-minute notification check: %#v, %v", state, err)
	}
	if f.restarter.calls != 0 || len(f.catalog.updateCalls) != 0 {
		t.Fatal("notify-only check changed runtime content")
	}
}

func waitForUpdateJobs(t *testing.T, service *jobs.Service) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		values, _, err := service.List(jobs.ListFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		pending := false
		for _, job := range values {
			pending = pending || job.Status == jobs.StatusQueued || job.Status == jobs.StatusRunning
		}
		if !pending {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Mod update jobs did not finish")
}

func (f *updateTestFixture) enableAutomatic(t *testing.T) {
	t.Helper()
	_, err := f.service.UpdatePolicy("room", PolicyInput{
		AutoCheck: true, AutoPrepare: true, ApplyWhenEmpty: true, GameAnnouncement: true,
		EmptyGraceSeconds: 60, CheckIntervalMinutes: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestManualCheckDoesNotDownloadOrRestart(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	fixture.enableAutomatic(t)
	state, err := fixture.service.check(context.Background(), "room", "job", false)
	if err != nil || state.Status != StatusAvailable || len(state.AvailableModIDs) != 2 {
		t.Fatalf("state = %#v, error = %v", state, err)
	}
	if len(fixture.catalog.updateCalls) != 0 || fixture.restarter.calls != 0 {
		t.Fatalf("manual check mutated content: updates=%v restarts=%d", fixture.catalog.updateCalls, fixture.restarter.calls)
	}
}

func TestInstallationRefreshRechecksRoomAndClearsResolvedUpdates(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	state, err := fixture.service.check(context.Background(), "room", "job", false)
	if err != nil || state.Status != StatusAvailable || len(state.AvailableModIDs) != 2 {
		t.Fatalf("initial state = %#v, error = %v", state, err)
	}
	fixture.catalog.updates = nil
	if err := fixture.service.RefreshInstallation(context.Background(), "agent:node", "native"); err != nil {
		t.Fatal(err)
	}
	waitForUpdateJobs(t, fixture.service.jobs)
	state, err = fixture.store.State("room")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusIdle || len(state.AvailableModIDs) != 0 || state.LastCheckedAt == nil {
		t.Fatalf("refreshed state = %#v", state)
	}
	if len(fixture.catalog.updateCalls) != 0 || fixture.restarter.calls != 0 {
		t.Fatalf("refresh mutated Mod content: updates=%v restarts=%d", fixture.catalog.updateCalls, fixture.restarter.calls)
	}
}

func TestInstallationRefreshDoesNotWaitForWorkshopCheck(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	fixture.catalog.checkStarted = make(chan struct{})
	fixture.catalog.checkContinue = make(chan struct{})
	defer close(fixture.catalog.checkContinue)
	returned := make(chan error, 1)
	go func() {
		returned <- fixture.service.RefreshInstallation(context.Background(), "agent:node", "native")
	}()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("content update waited for the follow-up Workshop check")
	}
	select {
	case <-fixture.catalog.checkStarted:
	case <-time.After(time.Second):
		t.Fatal("follow-up check was not started")
	}
	if err := fixture.service.RefreshInstallation(context.Background(), "agent:node", "native"); err != nil {
		t.Fatal(err)
	}
	_, total, err := fixture.service.jobs.List(jobs.ListFilter{Limit: 100})
	if err != nil || total != 1 {
		t.Fatalf("busy check was not coalesced: total=%d error=%v", total, err)
	}
}

func TestAutomaticUpdateWaitsForContinuousEmptyGraceBeforeRestarting(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	fixture.enableAutomatic(t)
	state, err := fixture.service.check(context.Background(), "room", "job", true)
	if err != nil || state.Status != StatusScheduled || state.EmptySince == nil || fixture.restarter.calls != 0 {
		t.Fatalf("prepared state = %#v, error = %v", state, err)
	}
	if len(fixture.catalog.updateCalls) != 2 {
		t.Fatalf("prepare calls: updates=%v", fixture.catalog.updateCalls)
	}
	fixture.now = fixture.now.Add(61 * time.Second)
	state, err = fixture.service.applyWhenEmpty(context.Background(), "room", "job")
	if err != nil || state.Status != StatusLoaded || fixture.restarter.calls != 1 {
		t.Fatalf("loaded state = %#v, error = %v, restarts = %d", state, err, fixture.restarter.calls)
	}
}

func TestAutomaticUpdateNeverRestartsWhilePlayersAreOnline(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	fixture.enableAutomatic(t)
	fixture.presence.values = []players.PresenceSnapshot{{RoomID: "room", Fresh: true, Online: 2}}
	announcer := &updateTestAnnouncer{}
	fixture.service.ConfigureAnnouncer(announcer)
	state, err := fixture.service.check(context.Background(), "room", "job", true)
	if err != nil || state.Status != StatusWaitingForPlayers || state.OnlinePlayers != 2 || fixture.restarter.calls != 0 || announcer.calls != 1 {
		t.Fatalf("waiting state = %#v, error = %v, restarts = %d, announcements = %d", state, err, fixture.restarter.calls, announcer.calls)
	}
}

func TestAutomaticUpdateBlocksOnStalePresence(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	fixture.enableAutomatic(t)
	fixture.presence.values = []players.PresenceSnapshot{{RoomID: "room", Fresh: false, StaleOnline: 1, Online: 1}}
	state, err := fixture.service.check(context.Background(), "room", "job", true)
	if err == nil || state.Status != StatusBlocked || state.ErrorCode != "PLAYER_PRESENCE_STALE" || fixture.restarter.calls != 0 || state.NextActionAt == nil {
		t.Fatalf("blocked state = %#v, error = %v", state, err)
	}
}

func TestManualApplyRechecksRestartsAndClearsAvailableUpdates(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	state, err := fixture.service.applyNow(context.Background(), "room", "job")
	if err != nil || state.Status != StatusLoaded {
		t.Fatalf("loaded state = %#v, error = %v", state, err)
	}
	if len(state.AvailableModIDs) != 0 {
		t.Fatalf("available updates were not cleared: %v", state.AvailableModIDs)
	}
	if len(fixture.catalog.updateCalls) != 2 || fixture.restarter.calls != 1 {
		t.Fatalf("manual apply calls: updates=%v restarts=%d", fixture.catalog.updateCalls, fixture.restarter.calls)
	}

}

func TestManualApplyDoesNotRestartWhenWorkshopIsAlreadyCurrent(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	fixture.catalog.updates = nil
	state, err := fixture.service.applyNow(context.Background(), "room", "job")
	if err != nil || state.Status != StatusIdle || fixture.restarter.calls != 0 {
		t.Fatalf("idle state = %#v, error = %v, restarts=%d", state, err, fixture.restarter.calls)
	}
}

func TestPolicyRejectsRestartWithPlayers(t *testing.T) {
	fixture := newUpdateTestFixture(t)
	_, err := fixture.service.UpdatePolicy("room", PolicyInput{
		AutoCheck: true, AutoPrepare: true, ApplyWhenEmpty: true, RestartWithPlayers: true,
		EmptyGraceSeconds: 60, CheckIntervalMinutes: 60,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("restart-with-players error = %v", err)
	}
}

func TestRuntimeDownloadFailureDoesNotRestartWorlds(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(strconv.FormatBool(automatic), func(t *testing.T) {
			f := newUpdateTestFixture(t)
			f.enableAutomatic(t)
			f.catalog.updateErr = errors.New("1/2 个安装实例已就绪: agent:debian12/native 下载 Workshop 2823458540: I/O Operation Failed")
			var state State
			var err error
			if automatic {
				state, err = f.service.check(context.Background(), "room", "job", true)
			} else {
				state, err = f.service.applyNow(context.Background(), "room", "job")
			}
			if err == nil || state.ErrorCode != "MOD_RUNTIME_UPDATE_FAILED" ||
				!strings.Contains(state.ErrorMessage, "2823458540") || !strings.Contains(state.ErrorMessage, "I/O Operation Failed") ||
				f.restarter.calls != 0 || state.PreparedAt != nil || len(state.AvailableModIDs) != 2 {
				t.Fatalf("download failure lost or restarted worlds: %#v err=%v restarts=%d", state, err, f.restarter.calls)
			}
		})
	}
}

func TestCheckRetainsDownloadedUpdateUntilRestartEvenAfterServiceRecreation(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.enableAutomatic(t)
	state, err := f.service.check(context.Background(), "room", "job", true)
	if err != nil || state.PreparedAt == nil {
		t.Fatalf("prepare: %#v %v", state, err)
	}
	preparedAt, emptySince := *state.PreparedAt, *state.EmptySince
	service, err := NewService(f.store, updateTestRooms{}, f.catalog, f.catalog, f.restarter, f.presence, f.service.jobs)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return f.now }
	f.now = f.now.Add(30 * time.Second)
	state, err = service.check(context.Background(), "room", "job", false)
	if err != nil || state.PreparedAt == nil || !state.PreparedAt.Equal(preparedAt) ||
		state.EmptySince == nil || !state.EmptySince.Equal(emptySince) || len(state.AvailableModIDs) != 2 ||
		f.restarter.calls != 0 || len(f.catalog.updateCalls) != 2 {
		t.Fatalf("current disks erased pending restart: %#v %v", state, err)
	}
	f.now = f.now.Add(31 * time.Second)
	state, err = service.check(context.Background(), "room", "job", true)
	if err != nil || state.Status != StatusLoaded || f.restarter.calls != 1 || len(f.catalog.updateCalls) != 2 {
		t.Fatalf("pending update did not finish: %#v %v", state, err)
	}
}

func TestManualRetryAfterRestartFailureUsesPreparedNodeContent(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.restarter.err = errors.New("Caves: startup failed")
	state, err := f.service.applyNow(context.Background(), "room", "job")
	if err == nil || state.ErrorCode != "MOD_WORLD_RESTART_FAILED" || state.PreparedAt == nil {
		t.Fatalf("failure: %#v %v", state, err)
	}
	f.restarter.err = nil
	state, err = f.service.applyNow(context.Background(), "room", "retry")
	if err != nil || state.Status != StatusLoaded || f.restarter.calls != 2 || len(f.catalog.updateCalls) != 2 {
		t.Fatalf("retry forgot restart or downloaded again: %#v %v", state, err)
	}
}

func TestLegacyPreparedPlanMustUpdateNodeBeforeRestart(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.enableAutomatic(t)
	_, err := f.store.SaveState(State{RoomID: "room", Status: StatusPrepared, PreparedPlanHash: "old-controller-plan", PreparedAt: &f.now})
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.service.applyWhenEmpty(context.Background(), "room", "job")
	if err != nil || len(f.catalog.updateCalls) != 2 || state.PreparedPlanHash != "" || state.Status != StatusScheduled || f.restarter.calls != 0 {
		t.Fatalf("legacy plan bypassed node download: %#v %v", state, err)
	}
}

func TestNewWorkshopReleaseWhileWaitingDownloadsAgainBeforeEmptyGrace(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.enableAutomatic(t)
	if _, err := f.service.check(context.Background(), "room", "job", true); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.catalog.updates = []string{"378160973"}
	state, err := f.service.applyWhenEmpty(context.Background(), "room", "job")
	if err != nil || state.Status != StatusPrepared || state.EmptySince != nil ||
		state.NextActionAt == nil || len(f.catalog.updateCalls) != 3 || f.restarter.calls != 0 {
		t.Fatalf("new download counted as grace or restarted: %#v %v", state, err)
	}
}

func TestJoiningPlayerDuringFinalVersionCheckCancelsAutomaticRestart(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.enableAutomatic(t)
	if _, err := f.service.check(context.Background(), "room", "job", true); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.presence.values = []players.PresenceSnapshot{{Fresh: true}, {Fresh: true, Online: 1}}
	state, err := f.service.applyWhenEmpty(context.Background(), "room", "job")
	if err != nil || state.Status != StatusWaitingForPlayers || state.EmptySince != nil || f.restarter.calls != 0 {
		t.Fatalf("joining player was ignored: %#v %v", state, err)
	}
}

func TestDisabledModsDoNotTriggerRestart(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.catalog.disabled = true
	state, err := f.service.applyNow(context.Background(), "room", "job")
	if err != nil || state.Status != StatusIdle || f.restarter.calls != 0 {
		t.Fatalf("disabled Mods restarted worlds: %#v %v", state, err)
	}
}

func TestStalePresenceInterruptsEmptyGrace(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.enableAutomatic(t)
	if _, err := f.service.check(context.Background(), "room", "job", true); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.presence.values = []players.PresenceSnapshot{{Fresh: false}}
	state, err := f.service.applyWhenEmpty(context.Background(), "room", "job")
	if err == nil || state.EmptySince != nil || f.restarter.calls != 0 {
		t.Fatalf("stale state counted as empty: %#v %v", state, err)
	}
	f.presence.values = nil
	state, err = f.service.applyWhenEmpty(context.Background(), "room", "retry")
	if err != nil || state.Status != StatusScheduled || f.restarter.calls != 0 {
		t.Fatalf("empty grace not restarted: %#v %v", state, err)
	}
}

func TestUpdateJobReturnsRuntimeDownloadErrorToUser(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.catalog.updateErr = errors.New("agent:debian12/native Workshop 2823458540: I/O Operation Failed")
	job, err := f.service.SubmitApplyNow("room")
	if err != nil {
		t.Fatal(err)
	}
	waitForUpdateJobs(t, f.service.jobs)
	job, err = f.service.jobs.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusFailed || job.Error == nil ||
		job.Error.Code != "MOD_RUNTIME_UPDATE_FAILED" || !strings.Contains(job.Error.Message, "2823458540") || f.restarter.calls != 0 {
		t.Fatalf("user-facing job lost download failure: %#v", job)
	}
}
