package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dont/internal/notifications"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

func TestRoomNotificationHTTPFeedHistoryAndSSEAdmission(t *testing.T) {
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "notices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	store := notifications.NewStore(db, "test_", t.TempDir())
	if err = store.Migrate(); err != nil {
		t.Fatal(err)
	}
	svc, err := notifications.New(store, notificationTestMail{})
	if err != nil {
		t.Fatal(err)
	}
	svc.Emit(notifications.Event{ID: "example", RoomID: "example", Kind: "world.season", Category: "season", Title: "季节变化", Message: "合成示例"})
	svc.Emit(notifications.Event{ID: "other", RoomID: "other", Kind: "world.season", Category: "season", Message: "private-other-room"})
	h := NewNotificationsHandler(svc)
	h.ConfigureRooms(func(room string) error {
		if room == "example" {
			return nil
		}
		return errors.New("not found")
	})
	router := gin.New()
	h.Register(router.Group("/api/v2"))
	request := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		router.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/v2/rooms/example/notifications/events", "/api/v2/rooms/example/recent-events"} {
		w := request(path)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "合成示例") || strings.Contains(w.Body.String(), "private-other-room") {
			t.Fatal(w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing cache control")
		}
	}
	if w := request("/api/v2/rooms/example/notifications/events?tail=true"); strings.Contains(w.Body.String(), "合成示例") {
		t.Fatal("tail replayed history")
	}
	if w := request("/api/v2/rooms/other/recent-events"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request("/api/v2/rooms/example/notifications/events?limit=101"); w.Code != 422 {
		t.Fatal("unbounded feed")
	}
	for i := 0; i < cap(h.streams); i++ {
		h.streams <- struct{}{}
	}
	if w := request("/api/v2/rooms/example/notifications/stream"); w.Code != 429 {
		t.Fatal("stream budget not enforced", w.Code)
	}
	for len(h.streams) > 0 {
		<-h.streams
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v2/rooms/example/notifications/stream", nil).WithContext(ctx)
	router.ServeHTTP(w, r)
	if len(h.streams) != 0 {
		t.Fatal("stream slot leaked")
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(w.Body.String(), "notice.page") {
		t.Fatal("no initial SSE cursor", w.Body.String())
	}
}
