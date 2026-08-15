package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/mods"
	"dont/internal/runtimeguard"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type modHandlerService struct{}

type modPlacementReaderFixture struct{}

func (modPlacementReaderFixture) RoomList(context.Context, string) (mods.ModList, error) {
	return mods.ModList{}, nil
}

func (modPlacementReaderFixture) ConfigurationFile(context.Context, string, string) (mods.ConfigurationFile, error) {
	return mods.ConfigurationFile{}, nil
}

func (modPlacementReaderFixture) Configuration(context.Context, string, string, string) (mods.ModConfiguration, error) {
	return mods.ModConfiguration{}, nil
}

func (modPlacementReaderFixture) PreviewConfiguration(context.Context, string, string, string, mods.ConfigUpdateRequest) (mods.ConfigPreview, error) {
	return mods.ConfigPreview{}, nil
}

func (modHandlerService) Search(_ context.Context, options mods.SearchOptions) (mods.SearchResult, error) {
	if !mods.ValidID(options.Query) && options.Query != "Global" && options.Query != "" {
		return mods.SearchResult{}, &mods.FieldError{Fields: map[string]string{"query": "无效搜索"}}
	}
	return mods.SearchResult{Items: []mods.SteamMod{{ID: "378160973", Name: "Global Positions"}}, Total: 1, Page: options.Page, PageSize: options.PageSize}, nil
}

func (modHandlerService) List(context.Context, string) (mods.ModList, error) {
	return mods.ModList{Items: []mods.ModState{{SteamMod: mods.SteamMod{ID: "378160973", Name: "Global Positions"}, Health: mods.HealthHealthy}}, Total: 1}, nil
}

func (modHandlerService) Library(context.Context) (mods.ModList, error) {
	return mods.ModList{Items: []mods.ModState{{SteamMod: mods.SteamMod{ID: "378160973", Name: "Global Positions"}, Downloaded: true, Health: mods.HealthHealthy}}, Total: 1}, nil
}

func (modHandlerService) Download(_ context.Context, request mods.DownloadRequest, _ io.Writer) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{request.ModID}, Message: "下载完成"}, nil
}

func (modHandlerService) AddToRoom(_ context.Context, _ string, _ string, modID string, _ mods.AddToRoomRequest) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{modID}, ProtectionBackupID: "backup", Message: "添加完成"}, nil
}

func (modHandlerService) UpdateLibrary(_ context.Context, modID string, _ io.Writer) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{modID}, Message: "更新完成"}, nil
}

func (modHandlerService) Install(_ context.Context, _ string, _ string, request mods.InstallRequest, _ io.Writer) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{request.ModID}, ProtectionBackupID: "backup", Message: "安装完成"}, nil
}

func (modHandlerService) Update(_ context.Context, _ string, modID string, _ io.Writer) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{modID}, Message: "更新完成"}, nil
}

func (modHandlerService) Enable(_ context.Context, _ string, _ string, modID string, _ mods.EnableRequest) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{modID}, Message: "启用完成"}, nil
}

func (modHandlerService) Repair(_ context.Context, _ string, modID string, _ mods.ModActionRequest, _ io.Writer) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{modID}, Message: "修复完成"}, nil
}

func (modHandlerService) Uninstall(_ context.Context, _ string, _ string, modID string, _ mods.ModActionRequest) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{modID}, Message: "卸载完成"}, nil
}

func (modHandlerService) CheckUpdates(context.Context, string) (mods.ActionResult, error) {
	return mods.ActionResult{ModIDs: []string{}, Message: "没有更新"}, nil
}

func (modHandlerService) ConfigurationFile(roomID, worldID string) (mods.ConfigurationFile, error) {
	return mods.ConfigurationFile{
		RoomID: roomID, WorldID: worldID, FileName: "modoverrides.lua", Content: "return {}\n",
		Exists: true, Revision: "revision", ReadAt: time.Now().UTC(),
	}, nil
}

func (modHandlerService) Configuration(_ context.Context, roomID, worldID, modID string) (mods.ModConfiguration, error) {
	return mods.ModConfiguration{Revision: "current", RoomID: roomID, WorldID: worldID, ModID: modID, Fields: []mods.ConfigField{}, Values: map[string]interface{}{}, UnknownValues: map[string]interface{}{}}, nil
}

func (modHandlerService) PreviewConfiguration(_ context.Context, _, _, _ string, request mods.ConfigUpdateRequest) (mods.ConfigPreview, error) {
	if request.ExpectedRevision != "current" {
		return mods.ConfigPreview{}, &mods.RevisionConflictError{CurrentRevision: "current"}
	}
	return mods.ConfigPreview{Revision: "current", NextRevision: "next", Changes: []mods.ConfigChange{{Path: "enabled", Operation: "replace"}}, RawPreserved: true}, nil
}

func (modHandlerService) ApplyConfiguration(context.Context, string, string, string, string, mods.ConfigUpdateRequest) (mods.ConfigApplyResult, error) {
	return mods.ConfigApplyResult{Revision: "next", ProtectionBackupID: "backup", Changes: []mods.ConfigChange{{Path: "enabled"}}}, nil
}

func newModHandlerApp(t *testing.T) (*gin.Engine, *jobs.Service) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "mods_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewModHandler(modHandlerService{}, jobService).Register(v2)
	NewJobHandler(jobService).Register(v2)
	return router, jobService
}

func TestModHTTPReadEndpointsAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, _ := newModHandlerApp(t)
	response := performJSON(router, http.MethodGet, "/api/v2/mods/search?query=Global&page=1&pageSize=20", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["total"] != float64(1) {
		t.Fatalf("unexpected search response: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/mods", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/mods/library", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/mods/actions/install", map[string]interface{}{"modId": "../bad"}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_MOD_ID")
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/worlds/world/mods/378160973/configuration", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/worlds/world/mods/configuration-file", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["content"] != "return {}\n" {
		t.Fatalf("unexpected configuration file response: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/worlds/world/mods/378160973/configuration/preview", map[string]interface{}{
		"expectedRevision": "stale", "enabled": true, "patch": map[string]interface{}{},
	}, nil, "")
	assertAPIError(t, response, http.StatusConflict, "CONFIG_REVISION_CONFLICT")
}

func TestRemoteMutationUsesStableHTTPAndJobErrorCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guardErr := fmt.Errorf("wrapped: %w", runtimeguard.ErrRemoteMutationUnavailable)
	if code := backupErrorCode(guardErr, "BACKUP_CREATE_FAILED"); code != runtimeguard.ErrorCode {
		t.Fatalf("backup job code = %q", code)
	}
	if jobErr := modJobError(guardErr); jobErr.Code != runtimeguard.ErrorCode {
		t.Fatalf("Mod job error = %#v", jobErr)
	}
	if code := saveImportErrorCode(guardErr); code != runtimeguard.ErrorCode {
		t.Fatalf("save import job code = %q", code)
	}

	router := gin.New()
	router.GET("/backup", func(c *gin.Context) { backupFailure(c, guardErr) })
	router.GET("/mods", func(c *gin.Context) { modFailure(c, guardErr) })
	router.GET("/save-import", func(c *gin.Context) { saveImportFailure(c, guardErr) })
	for _, path := range []string{"/backup", "/mods", "/save-import"} {
		response := performJSON(router, http.MethodGet, path, nil, nil, "")
		assertAPIError(t, response, http.StatusConflict, runtimeguard.ErrorCode)
	}
}

func TestModHTTPActionsUseJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, jobService := newModHandlerApp(t)
	response := performJSON(router, http.MethodPost, "/api/v2/mods/library/actions/download", map[string]interface{}{
		"modId": "378160973", "includeDependencies": true,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID, _ := responseData(t, response)["id"].(string)
	job := waitForModJob(t, jobService, jobID)
	if job.Kind != "mod.download" || job.RoomID != "" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected node download job: %#v", job)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/mods/378160973/actions/add", map[string]interface{}{
		"worldIds": []string{"world"}, "enabled": true, "includeDependencies": true,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job = waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "mod.room.add" || job.RoomID != "room" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected room add job: %#v", job)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/mods/378160973/actions/enable", map[string]interface{}{
		"worldIds": []string{"world"}, "enabled": true,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID, _ = responseData(t, response)["id"].(string)
	job = waitForModJob(t, jobService, jobID)
	if job.Kind != "mod.enable" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected Mod job: %#v", job)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/worlds/world/mods/378160973/configuration/actions/apply", map[string]interface{}{
		"expectedRevision": "current", "enabled": false, "patch": map[string]interface{}{},
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job = waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "mod.configuration.apply" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected configuration job: %#v", job)
	}
}

func TestPlacementAwareModHandlerRejectsLegacyRoomMutations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "mods_placement_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewModHandler(modHandlerService{}, jobService)
	handler.ConfigurePlacementReader(modPlacementReaderFixture{})
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	requests := []struct {
		path string
		body interface{}
	}{
		{"/api/v2/rooms/room/mods/378160973/actions/add", map[string]interface{}{}},
		{"/api/v2/rooms/room/mods/actions/install", map[string]interface{}{}},
		{"/api/v2/rooms/room/mods/378160973/actions/update", nil},
		{"/api/v2/rooms/room/mods/378160973/actions/enable", map[string]interface{}{}},
		{"/api/v2/rooms/room/mods/378160973/actions/repair", map[string]interface{}{}},
		{"/api/v2/rooms/room/mods/378160973/actions/uninstall", map[string]interface{}{}},
		{"/api/v2/rooms/room/worlds/world/mods/378160973/configuration/actions/apply", map[string]interface{}{}},
	}
	for _, item := range requests {
		response := performJSON(router, http.MethodPost, item.path, item.body, nil, "")
		assertAPIError(t, response, http.StatusConflict, "MOD_PUBLICATION_REQUIRED")
	}

	response := performJSON(router, http.MethodPost, "/api/v2/mods/library/378160973/actions/update", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
}

func waitForModJob(t *testing.T, service *jobs.Service, jobID string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(jobID)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusCanceled) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Mod job %q did not finish", jobID)
	return jobs.Job{}
}
