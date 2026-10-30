package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

func TestScopedBearerCannotUseRESTOrLegacyBypass(t *testing.T) {
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := aicontrol.NewStore(db, "scope_", t.TempDir())
	if err = store.Migrate(); err != nil {
		t.Fatal(err)
	}
	_, secret, err := store.CreateToken(aicontrol.TokenInput{Name: "reader", Days: 1, Profile: aicontrol.ProfileGameRead, RoomIDs: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api", AITokenScope(store, catalog, nil))
	reached := 0
	handler := func(c *gin.Context) { reached++; c.Status(200) }
	api.GET("/v2/rooms/:roomId/players", handler)
	api.POST("/v2/rooms/:roomId/actions/:action", handler)
	api.GET("/v2/jobs/:jobId", handler)
	api.GET("/v1/config", handler)
	api.GET("/v2/system/ai/tokens", handler)
	cases := []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/v2/rooms/a/players?includeAccessLists=false", 200},
		{"GET", "/api/v2/rooms/b/players?includeAccessLists=false", 403},
		{"GET", "/api/v2/rooms/a/players", 403},
		{"POST", "/api/v2/rooms/a/actions/save", 403},
		{"GET", "/api/v2/jobs/other", 403}, {"GET", "/api/v1/config", 403},
		{"GET", "/api/v2/system/ai/tokens", 403},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if reached != 1 {
		t.Fatalf("denied route reached handler: %d", reached)
	}
	// Cookie sessions keep their existing admin behavior.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if w.Code != 200 {
		t.Fatal("scope middleware changed cookie path")
	}
}
