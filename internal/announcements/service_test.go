package announcements

import (
	"errors"
	"testing"
	"time"
)

func TestServiceAnnouncementLifecycleAndStatus(t *testing.T) {
	service, err := NewService(newAnnouncementStore(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	created, err := service.Create(Input{
		Title: "  开服公告  ", Content: "  欢迎来到服务器  ", ExpireTime: now.Add(time.Hour),
		Target: TargetOnline, Important: true,
	})
	if err != nil || created.Title != "开服公告" || created.Content != "欢迎来到服务器" || created.Status != StatusActive {
		t.Fatalf("create = %#v, %v", created, err)
	}
	updated, err := service.Update(created.ID, Input{
		Title: "活动公告", Content: "活动开始", ExpireTime: now.Add(2 * time.Hour), Target: TargetAll,
	})
	if err != nil || updated.PublishTime != created.PublishTime || updated.Important || updated.Target != TargetAll {
		t.Fatalf("update = %#v, %v", updated, err)
	}
	service.now = func() time.Time { return now.Add(3 * time.Hour) }
	items, err := service.List()
	if err != nil || len(items) != 1 || items[0].Status != StatusExpired {
		t.Fatalf("expired list = %#v, %v", items, err)
	}
	if err := service.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted get error = %v", err)
	}
}

func TestServiceRejectsInvalidAnnouncementInput(t *testing.T) {
	service, err := NewService(newAnnouncementStore(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	_, err = service.Create(Input{Title: "x", Content: "", ExpireTime: now, Target: "unknown"})
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || len(fieldErr.Fields) != 4 {
		t.Fatalf("invalid input error = %#v", err)
	}
}
