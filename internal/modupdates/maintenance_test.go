package modupdates

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"dont/internal/maintenance"
)

type sharedRoomCatalog struct{ *updateTestCatalog }

func (c sharedRoomCatalog) AffectedRoomIDs(context.Context, string) ([]string, error) {
	return c.installationRooms, nil
}

type maintenanceCounter func(context.Context, string) (int, error)

func (f maintenanceCounter) OnlinePlayers(ctx context.Context, id string) (int, error) {
	return f(ctx, id)
}

type maintenanceRestarter struct {
	rooms  []string
	before func()
}

func (r *maintenanceRestarter) RestartRunningWorlds(ctx context.Context, roomID, _ string) error {
	if r.before != nil {
		r.before()
	}
	if err := maintenance.Check(ctx); err != nil {
		return err
	}
	r.rooms = append(r.rooms, roomID)
	return nil
}

func TestAutomaticModsWaitForAllSharedRoomsBeforeDownload(t *testing.T) {
	for _, condition := range []string{"online", "offline"} {
		t.Run(condition, func(t *testing.T) {
			f := newUpdateTestFixture(t)
			f.enableAutomatic(t)
			f.catalog.installationRooms = []string{"room", "shared"}
			f.service.catalog = sharedRoomCatalog{f.catalog}
			f.service.ConfigureOnlineCounter(maintenanceCounter(func(_ context.Context, roomID string) (int, error) {
				if roomID == "shared" {
					if condition == "offline" {
						return 0, errors.New("Agent unavailable")
					}
					return 1, nil
				}
				return 0, nil
			}))
			state, err := f.service.check(context.Background(), "room", "job", true)
			if len(f.catalog.updateCalls) != 0 || f.restarter.calls != 0 || state.EmptySince != nil {
				t.Fatalf("shared players ignored: state=%+v err=%v", state, err)
			}
			if condition == "online" && (err != nil || state.Status != StatusWaitingForPlayers) {
				t.Fatalf("%+v %v", state, err)
			}
			if condition == "offline" && (err == nil || state.Status != StatusBlocked) {
				t.Fatalf("%+v %v", state, err)
			}
		})
	}
}

func TestAutomaticModsRestartSharedRoomsAndRecheckAfterNotification(t *testing.T) {
	for _, condition := range []string{"empty", "joining", "paused"} {
		t.Run(condition, func(t *testing.T) {
			f := newUpdateTestFixture(t)
			f.enableAutomatic(t)
			f.catalog.installationRooms = []string{"room", "shared"}
			f.service.catalog = sharedRoomCatalog{f.catalog}
			count := 0
			f.service.ConfigureOnlineCounter(maintenanceCounter(func(context.Context, string) (int, error) { return count, nil }))
			restarter := &maintenanceRestarter{}
			f.service.restarter = restarter
			if _, err := f.service.check(context.Background(), "room", "job", true); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(time.Minute)
			restarter.before = func() {
				if condition == "joining" {
					count = 1
				}
				if condition == "paused" {
					policy, err := f.store.Policy("room")
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.service.UpdatePolicy("room", PolicyInput{ExpectedRevision: policy.Revision, EmptyGraceSeconds: 60, CheckIntervalMinutes: 15})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			state, err := f.service.applyWhenEmpty(context.Background(), "room", "job")
			if condition == "empty" {
				if err != nil || state.Status != StatusLoaded || !reflect.DeepEqual(restarter.rooms, []string{"room", "shared"}) {
					t.Fatalf("shared restart: %+v %v rooms=%v", state, err, restarter.rooms)
				}
			} else if err == nil || len(restarter.rooms) != 0 || state.PreparedAt == nil {
				t.Fatalf("late change ignored: %+v %v rooms=%v", state, err, restarter.rooms)
			}
		})
	}
}

func TestAutomaticModsRecheckBeforeDownloadingNewVersionAfterGrace(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.enableAutomatic(t)
	if _, err := f.service.check(context.Background(), "room", "job", true); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.catalog.updates = []string{"378160973"}
	calls := 0
	f.service.ConfigureOnlineCounter(maintenanceCounter(func(context.Context, string) (int, error) {
		calls++
		if calls > 1 {
			return 1, nil
		}
		return 0, nil
	}))
	state, err := f.service.applyWhenEmpty(context.Background(), "room", "job")
	if err != nil || state.Status != StatusWaitingForPlayers || len(f.catalog.updateCalls) != 2 || f.restarter.calls != 0 {
		t.Fatalf("download with joining player: %+v %v calls=%v", state, err, f.catalog.updateCalls)
	}
}

func TestNestedMaintenanceDoesNotExpandHeldLocks(t *testing.T) {
	f := newUpdateTestFixture(t)
	f.service.catalog = sharedRoomCatalog{f.catalog}
	ctx, release, _, err := f.service.lockAffectedRooms(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.catalog.installationRooms = []string{"room", "new-shared-room"}
	if _, _, _, err := f.service.lockAffectedRooms(ctx, "room"); err == nil {
		t.Fatal("nested operation acquired newly shared rooms out of lock order")
	}
}
