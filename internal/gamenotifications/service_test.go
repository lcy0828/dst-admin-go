package gamenotifications

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/shared"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type notificationTestCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

type notificationMultiCatalog struct {
	rooms  map[string]rooms.Room
	worlds map[string][]rooms.World
}

func (c notificationMultiCatalog) Room(roomID string) (rooms.Room, error) {
	room, exists := c.rooms[roomID]
	if !exists {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return room, nil
}

func (c notificationMultiCatalog) Worlds(roomID string) ([]rooms.World, error) {
	worlds, exists := c.worlds[roomID]
	if !exists {
		return nil, rooms.ErrRoomNotFound
	}
	return append([]rooms.World(nil), worlds...), nil
}

func (c notificationTestCatalog) Room(roomID string) (rooms.Room, error) {
	if roomID != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}

func (c notificationTestCatalog) Worlds(roomID string) ([]rooms.World, error) {
	if roomID != c.room.ID {
		return nil, rooms.ErrRoomNotFound
	}
	return append([]rooms.World(nil), c.worlds...), nil
}

type notificationTestRuntime struct {
	statuses   map[string]shared.ShardRuntimeStatus
	statusErrs map[string]error
	sendErrs   map[string]error
	commands   map[string]string
}

func (r *notificationTestRuntime) Status(_ context.Context, _, worldID string) (shared.ShardRuntimeStatus, error) {
	return r.statuses[worldID], r.statusErrs[worldID]
}

func (r *notificationTestRuntime) SendID(_ context.Context, _, worldID string, request shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error) {
	if r.commands == nil {
		r.commands = make(map[string]string)
	}
	r.commands[worldID] = request.Command
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	return shared.RuntimeOperationResult{
		TargetID: "agent:node-a", AgentID: "node-a", TopologyRevision: "revision-1",
		ObservedAt: now, Message: "控制台已接收",
	}, r.sendErrs[worldID]
}

type notificationTestOnline struct {
	count int
	err   error
}

func (o notificationTestOnline) OnlinePlayers(context.Context, string) (int, error) {
	return o.count, o.err
}

func newNotificationTestService(t *testing.T) (*Service, *Store, *notificationTestRuntime, notificationTestCatalog) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "notification_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"notification_test_game_notification",
		"notification_test_game_notification_delivery",
		"notification_test_game_notification_policy",
	} {
		if !db.NewScope(notificationRecord{}).Dialect().HasTable(table) {
			t.Fatalf("migration did not create %s", table)
		}
	}
	roomID := rooms.EncodeID("room1")
	catalog := notificationTestCatalog{
		room: rooms.Room{ID: roomID, DirectoryName: "room1", Name: "测试房间", Managed: true},
		worlds: []rooms.World{
			{ID: rooms.EncodeID("Master"), RoomID: roomID, DirectoryName: "Master", Name: "森林"},
			{ID: rooms.EncodeID("Caves"), RoomID: roomID, DirectoryName: "Caves", Name: "洞穴"},
		},
	}
	runtime := &notificationTestRuntime{
		statuses: map[string]shared.ShardRuntimeStatus{
			catalog.worlds[0].ID: {State: "running", SessionExists: true},
			catalog.worlds[1].ID: {State: "stopped"},
		},
		statusErrs: make(map[string]error), sendErrs: make(map[string]error),
	}
	service, err := NewService(catalog, runtime, store)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, runtime, catalog
}

func TestServiceSendsToRunningShardsAndRecordsSkippedDeliveries(t *testing.T) {
	service, store, runtime, catalog := newNotificationTestService(t)
	plan, err := service.Prepare(catalog.room.ID, "  今晚 \"十点\" 重启\n请及时保存  ", SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := plan.Runner("job-1")(context.Background(), func(result jobs.TargetResult) {
		results = append(results, result)
	}); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != jobs.StatusSucceeded || results[1].Status != jobs.StatusSucceeded {
		t.Fatalf("results=%#v", results)
	}
	wantCommand := "c_announce(\"今晚 \\\"十点\\\" 重启\\n请及时保存\")"
	if runtime.commands[catalog.worlds[0].ID] != wantCommand {
		t.Fatalf("command=%q want=%q", runtime.commands[catalog.worlds[0].ID], wantCommand)
	}
	if _, exists := runtime.commands[catalog.worlds[1].ID]; exists {
		t.Fatal("stopped Shard received a notification command")
	}
	value, err := store.Get(plan.Notification.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != StatusSucceeded || value.JobID != "job-1" || value.SuccessCount != 1 || value.SkippedCount != 1 || len(value.Deliveries) != 2 {
		t.Fatalf("notification=%#v", value)
	}
	if value.Deliveries[0].TargetID != "agent:node-a" || value.Deliveries[0].AgentID != "node-a" || value.Deliveries[1].Status != DeliverySkipped {
		t.Fatalf("deliveries=%#v", value.Deliveries)
	}
}

func TestServiceRecordsFailuresAndCancellation(t *testing.T) {
	service, store, runtime, catalog := newNotificationTestService(t)
	runtime.sendErrs[catalog.worlds[0].ID] = errors.New("console unavailable")
	plan, err := service.Prepare(catalog.room.ID, "维护通知", SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Runner("job-failed")(context.Background(), func(jobs.TargetResult) {}); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Get(plan.Notification.ID)
	if err != nil || failed.Status != StatusFailed || failed.FailureCount != 1 || failed.SkippedCount != 1 {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}

	canceledPlan, err := service.Prepare(catalog.room.ID, "取消通知", SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var canceledResults []jobs.TargetResult
	err = canceledPlan.Runner("job-canceled")(ctx, func(result jobs.TargetResult) {
		canceledResults = append(canceledResults, result)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	canceled, err := store.Get(canceledPlan.Notification.ID)
	if err != nil || canceled.Status != StatusCanceled || canceled.CanceledCount != 2 || len(canceledResults) != 2 {
		t.Fatalf("canceled=%#v results=%#v err=%v", canceled, canceledResults, err)
	}
}

func TestStoreRecoversInterruptedNotificationHistory(t *testing.T) {
	service, store, _, catalog := newNotificationTestService(t)
	plan, err := service.Prepare(catalog.room.ID, "服务重启测试", SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJob(plan.Notification.ID, "interrupted-job"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSending(plan.Notification.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Get(plan.Notification.ID)
	if err != nil || recovered.Status != StatusFailed || recovered.FailureCount != len(catalog.worlds) || recovered.CompletedAt == nil {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	for _, delivery := range recovered.Deliveries {
		if delivery.Status != DeliveryFailed || delivery.ErrorCode != "SERVICE_RESTARTED" {
			t.Fatalf("delivery=%#v", delivery)
		}
	}
	if err := store.RecoverInterrupted(); err != nil {
		t.Fatalf("second recovery must be idempotent: %v", err)
	}
}

func TestServicePolicyAndLifecycleCountdown(t *testing.T) {
	service, _, runtime, catalog := newNotificationTestService(t)
	policy, err := service.Policy(catalog.room.ID)
	if err != nil || !policy.Enabled || policy.CountdownSeconds != 60 || policy.UpdatedAt.IsZero() {
		t.Fatalf("default policy=%#v err=%v", policy, err)
	}
	if _, err := service.SavePolicy(catalog.room.ID, PolicyInput{Enabled: true, CountdownSeconds: 9}); err == nil {
		t.Fatal("invalid countdown was accepted")
	}
	policy, err = service.SavePolicy(catalog.room.ID, PolicyInput{Enabled: true, CountdownSeconds: 10})
	if err != nil || policy.CountdownSeconds != 10 {
		t.Fatalf("saved policy=%#v err=%v", policy, err)
	}
	service.ConfigureOnlineCounter(notificationTestOnline{count: 1})
	var waits []time.Duration
	service.wait = func(_ context.Context, duration time.Duration) error {
		waits = append(waits, duration)
		return nil
	}
	if err := service.BeforeOperation(context.Background(), catalog.room.ID, "stop", "", "room-job-1"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waits, []time.Duration{10 * time.Second}) {
		t.Fatalf("waits=%v", waits)
	}
	if command := runtime.commands[catalog.worlds[0].ID]; !strings.Contains(command, "服务器将在 10 秒后停止") {
		t.Fatalf("countdown command=%q", command)
	}
	list, err := service.List(ListFilter{RoomID: catalog.room.ID})
	if err != nil || list.Total != 1 || list.Items[0].Source != SourceRoomStop || list.Items[0].JobID != "room-job-1" {
		t.Fatalf("history=%#v err=%v", list, err)
	}

	service.ConfigureOnlineCounter(notificationTestOnline{count: 0})
	if err := service.BeforeOperation(context.Background(), catalog.room.ID, "restart", "", "room-job-2"); err != nil {
		t.Fatal(err)
	}
	list, err = service.List(ListFilter{RoomID: catalog.room.ID})
	if err != nil || list.Total != 1 {
		t.Fatalf("offline countdown created history: %#v err=%v", list, err)
	}
}

func TestServiceCoordinatesMultiRoomCountdownAndPreservesTriggerSource(t *testing.T) {
	service, _, runtime, catalog := newNotificationTestService(t)
	secondRoomID := rooms.EncodeID("room2")
	secondWorldID := rooms.EncodeID("Moon")
	secondRoom := rooms.Room{ID: secondRoomID, DirectoryName: "room2", Name: "第二房间", Managed: true}
	secondWorld := rooms.World{ID: secondWorldID, RoomID: secondRoomID, DirectoryName: "Moon", Name: "月岛"}
	service.rooms = notificationMultiCatalog{
		rooms: map[string]rooms.Room{catalog.room.ID: catalog.room, secondRoomID: secondRoom},
		worlds: map[string][]rooms.World{
			catalog.room.ID: catalog.worlds,
			secondRoomID:    {secondWorld},
		},
	}
	runtime.statuses[secondWorldID] = shared.ShardRuntimeStatus{State: "running", SessionExists: true}
	service.ConfigureOnlineCounter(notificationTestOnline{count: 1})
	if _, err := service.SavePolicy(catalog.room.ID, PolicyInput{Enabled: true, CountdownSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SavePolicy(secondRoomID, PolicyInput{Enabled: true, CountdownSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	service.wait = func(_ context.Context, duration time.Duration) error {
		waits = append(waits, duration)
		return nil
	}
	if err := service.BeforeOperations(context.Background(), []string{catalog.room.ID, secondRoomID}, "restart", "game_update", "update-job"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waits, []time.Duration{30 * time.Second, 20 * time.Second, 10 * time.Second}) {
		t.Fatalf("waits=%v", waits)
	}
	list, err := service.List(ListFilter{})
	if err != nil || list.Total != 5 {
		t.Fatalf("history=%#v err=%v", list, err)
	}
	for _, notification := range list.Items {
		if notification.Source != SourceGameUpdate || notification.JobID != "update-job" {
			t.Fatalf("notification=%#v", notification)
		}
	}
}

func TestServiceSendsAutomationNotificationThroughHistory(t *testing.T) {
	service, _, _, catalog := newNotificationTestService(t)
	success, failure, skipped, err := service.SendAutomation(context.Background(), catalog.room.ID, "定时维护提醒", "automation-job")
	if err != nil || success != 1 || failure != 0 || skipped != 1 {
		t.Fatalf("success=%d failure=%d skipped=%d err=%v", success, failure, skipped, err)
	}
	list, err := service.List(ListFilter{RoomID: catalog.room.ID})
	if err != nil || list.Total != 1 || list.Items[0].Source != SourceAutomation || list.Items[0].JobID != "automation-job" {
		t.Fatalf("history=%#v err=%v", list, err)
	}
}

func TestServiceRejectsInvalidMessageAndUnmanagedRoom(t *testing.T) {
	service, _, _, catalog := newNotificationTestService(t)
	if _, err := service.Prepare(catalog.room.ID, strings.Repeat("界", MaxMessageRunes+1), SourceManual); err == nil {
		t.Fatal("oversized message was accepted")
	}
	service.rooms = notificationTestCatalog{room: rooms.Room{ID: catalog.room.ID, Managed: false}, worlds: catalog.worlds}
	if _, err := service.Prepare(catalog.room.ID, "测试", SourceManual); !errors.Is(err, ErrRoomUnmanaged) {
		t.Fatalf("unmanaged error=%v", err)
	}
}
