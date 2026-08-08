package httpapi

import (
	"net/http"
	"testing"
	"time"

	"dont/internal/containers"
	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func TestContainerHTTPListAndActions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "container_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, _ := jobs.NewService(store, jobs.NewBroker())
	service, _ := containers.NewService(containers.NewMemoryTransport(), jobService)
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewContainerHandler(service).Register(v2)
	NewJobHandler(jobService).Register(v2)

	response := performJSON(router, http.MethodGet, "/api/v2/containers", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["available"] != true || data["total"] != float64(2) {
		t.Fatalf("unexpected list: %s", response.Body.String())
	}
	const cavesID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	response = performJSON(router, http.MethodPost, "/api/v2/containers/"+cavesID+"/actions/start", map[string]string{"confirmation": ""}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID := responseData(t, response)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := jobService.Get(jobID)
		if job.Status == jobs.StatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	response = performJSON(router, http.MethodPost, "/api/v2/containers/"+cavesID+"/actions/remove", map[string]string{"confirmation": "wrong"}, nil, "")
	assertStatus(t, response, http.StatusConflict)
}
