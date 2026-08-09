package httpapi

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"dont/internal/announcements"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newAnnouncementHandlerApp(t *testing.T) *gin.Engine {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := announcements.NewStore(db, "announcement_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := announcements.NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewAnnouncementHandler(service).Register(router.Group("/api/v2"))
	return router
}

func TestAnnouncementHTTPCRUD(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newAnnouncementHandlerApp(t)
	response := performJSON(router, http.MethodGet, "/api/v2/announcements", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("announcement list is cacheable: %#v", response.Header())
	}

	response = performJSON(router, http.MethodPost, "/api/v2/announcements", map[string]interface{}{
		"title": "x", "content": "", "expireTime": time.Now().Add(-time.Hour), "target": "unknown",
	}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_ANNOUNCEMENT")

	expireTime := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	response = performJSON(router, http.MethodPost, "/api/v2/announcements", map[string]interface{}{
		"title": "开服公告", "content": "欢迎来到服务器", "expireTime": expireTime,
		"target": "all", "important": true,
	}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	created := responseData(t, response)
	id := uint64(created["id"].(float64))
	if id == 0 || created["status"] != announcements.StatusActive {
		t.Fatalf("unexpected created announcement: %s", response.Body.String())
	}

	path := "/api/v2/announcements/" + strconv.FormatUint(id, 10)
	response = performJSON(router, http.MethodGet, path, nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodPut, path, map[string]interface{}{
		"title": "更新公告", "content": "内容已更新", "expireTime": expireTime.Add(time.Hour),
		"target": "admins", "important": false,
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if updated := responseData(t, response); updated["title"] != "更新公告" || updated["target"] != "admins" || updated["important"] != false {
		t.Fatalf("unexpected updated announcement: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodDelete, path, nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, path, nil, nil, "")
	assertAPIError(t, response, http.StatusNotFound, "ANNOUNCEMENT_NOT_FOUND")
	response = performJSON(router, http.MethodGet, "/api/v2/announcements/not-a-number", nil, nil, "")
	assertAPIError(t, response, http.StatusBadRequest, "INVALID_ANNOUNCEMENT_ID")
}
