package announcements

import (
	"errors"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newAnnouncementStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "announcement_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if !db.NewScope(announcementRecord{}).Dialect().HasTable("announcement_test_announcement") {
		t.Fatal("announcement migration did not honor table prefix")
	}
	return store
}

func TestStoreAnnouncementCRUD(t *testing.T) {
	store := newAnnouncementStore(t)
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	created, err := store.Create(Announcement{
		Title: "维护通知", Content: "今晚维护", PublishTime: now, ExpireTime: now.Add(time.Hour),
		Target: TargetAll, Important: true, UpdatedAt: now,
	})
	if err != nil || created.ID == 0 {
		t.Fatalf("create = %#v, %v", created, err)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("list = %#v, %v", items, err)
	}
	created.Title = "维护完成"
	created.Target = TargetAdmins
	created.Important = false
	created.UpdatedAt = now.Add(time.Minute)
	updated, err := store.Update(created)
	if err != nil || updated.Title != "维护完成" || updated.Target != TargetAdmins || updated.Important {
		t.Fatalf("update = %#v, %v", updated, err)
	}
	loaded, err := store.Get(created.ID)
	if err != nil || loaded.UpdatedAt != created.UpdatedAt {
		t.Fatalf("get = %#v, %v", loaded, err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted get error = %v", err)
	}
	if err := store.Delete(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted delete error = %v", err)
	}
}
