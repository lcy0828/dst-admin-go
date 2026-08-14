package operationlease

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newLeaseTestService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(db, "lease_test_")
	if err := service.Migrate(); err != nil {
		t.Fatal(err)
	}
	return service
}

func TestLeaseIsIdempotentBusyAndMonotonicAfterExpiry(t *testing.T) {
	service := newLeaseTestService(t)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	first, err := service.Acquire(context.Background(), "room-a", "operation-a", time.Minute)
	if err != nil || first.FencingToken != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	repeated, err := service.Acquire(context.Background(), "room-a", "operation-a", time.Minute)
	if err != nil || repeated != first {
		t.Fatalf("repeated=%#v err=%v", repeated, err)
	}
	if _, err := service.Acquire(context.Background(), "room-a", "operation-b", time.Minute); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy error=%v", err)
	}
	now = now.Add(2 * time.Minute)
	second, err := service.Acquire(context.Background(), "room-a", "operation-b", time.Minute)
	if err != nil || second.FencingToken != 2 || second.LeaseID == first.LeaseID {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}

func TestLeaseRenewAndReleaseRequireCurrentFence(t *testing.T) {
	service := newLeaseTestService(t)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	lease, err := service.Acquire(context.Background(), "room-a", "operation-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	renewed, err := service.Renew(context.Background(), lease, 2*time.Minute)
	if err != nil || !renewed.ExpiresAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("renewed=%#v err=%v", renewed, err)
	}
	stale := renewed
	stale.FencingToken++
	if err := service.Release(stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale release error=%v", err)
	}
	if err := service.Release(renewed); err != nil {
		t.Fatal(err)
	}
	next, err := service.Acquire(context.Background(), "room-a", "operation-b", time.Minute)
	if err != nil || next.FencingToken != 2 {
		t.Fatalf("next=%#v err=%v", next, err)
	}
}
