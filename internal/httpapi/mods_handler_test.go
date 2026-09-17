package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/internal/runtimedriver"
	"dont/internal/runtimeguard"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type modHandlerService struct{}

func TestModConfigurationFailureReportsPartialWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/partial", func(c *gin.Context) {
		modConfigurationFailure(c, mods.ConfigApplyResult{PublishedTargets: 1},
			fmt.Errorf("write Caves: %w", &mods.RevisionConflictError{CurrentRevision: "current"}))
	})
	response := performJSON(router, http.MethodPost, "/partial", nil, nil, "")
	assertStatus(t, response, http.StatusConflict)
	if !strings.Contains(response.Body.String(), "MOD_CONFIGURATION_PARTIALLY_APPLIED") ||
		!strings.Contains(response.Body.String(), "已保存 1 个世界") || !strings.Contains(response.Body.String(), "Caves") {
		t.Fatalf("partial result was hidden: %s", response.Body.String())
	}
}

type modPlacementReaderFixture struct{}

type failingModPlacementReaderFixture struct {
	modPlacementReaderFixture
}

type outdatedModPlacementReaderFixture struct {
	modPlacementReaderFixture
}

type directModEnabledFixture struct {
	modPlacementReaderFixture
	request mods.EnableRequest
}

func (f *directModEnabledFixture) SetEnabled(_ context.Context, _, _ string, request mods.EnableRequest) (mods.ConfigApplyResult, error) {
	f.request = request
	return mods.ConfigApplyResult{
		WorldIDs: request.WorldIDs, PublishedTargets: len(request.WorldIDs),
		Revisions: map[string]string{"world": "next"}, Revision: "next",
	}, nil
}

type modInstallationUpdaterFixture struct {
	mu             sync.Mutex
	calls          int
	targetID       string
	installationID string
	modIDs         []string
}

type modInstallationRefresherFixture struct {
	mu             sync.Mutex
	calls          int
	targetID       string
	installationID string
	err            error
}

func (f *modInstallationUpdaterFixture) UpdateInstallationMods(ctx context.Context, targetID, installationID string, modIDs []string, _ io.Writer) (mods.ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	operationprogress.Report(ctx, operationprogress.Update{
		Stage: operationprogress.StageModCache, Percent: 50, Message: "正在下载机器模组",
		CurrentBytes: 32 << 20, TotalBytes: 92 << 20, BytesPerSecond: 4_500_375,
	})
	f.calls++
	f.targetID = targetID
	f.installationID = installationID
	f.modIDs = append([]string(nil), modIDs...)
	return mods.ActionResult{ModIDs: append([]string(nil), modIDs...), Message: "机器模组更新完成"}, nil
}

func (f *modInstallationUpdaterFixture) snapshot() (int, string, string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.targetID, f.installationID, append([]string(nil), f.modIDs...)
}

func (f *modInstallationRefresherFixture) RefreshInstallation(_ context.Context, targetID, installationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.targetID = targetID
	f.installationID = installationID
	return f.err
}

func (f *modInstallationRefresherFixture) snapshot() (int, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.targetID, f.installationID
}

func (failingModPlacementReaderFixture) InstallationModInventory(context.Context, string, string) (mods.RuntimeInstallationModInventory, error) {
	return mods.RuntimeInstallationModInventory{}, fmt.Errorf("运行机器离线")
}

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

func (modPlacementReaderFixture) ApplyConfiguration(_ context.Context, roomID, worldID, _ string, request mods.ConfigUpdateRequest) (mods.ConfigApplyResult, error) {
	worldIDs := append([]string(nil), request.WorldIDs...)
	if len(worldIDs) == 0 {
		worldIDs = []string{worldID}
	}
	return mods.ConfigApplyResult{Revision: "next", WorldIDs: worldIDs, PublishedTargets: len(worldIDs)}, nil
}

func (modPlacementReaderFixture) InstallationModInventory(_ context.Context, targetID, installationID string) (mods.RuntimeInstallationModInventory, error) {
	return mods.RuntimeInstallationModInventory{
		TargetID: targetID, InstallationID: installationID,
		Items: []mods.RuntimeInstallationMod{{
			SteamMod:       mods.SteamMod{ID: "378160973", Name: "Global Positions", Dependencies: []string{}, Tags: []string{}},
			CurrentVersion: "1.7.6", LatestVersion: "1.7.6", VersionStatus: "current", FileStatus: "ready",
			InstalledSize: 4096, RoomReferences: []mods.RuntimeModRoomReference{},
		}},
		Total: 1, Current: 1, ObservedAt: time.Now().UTC(),
	}, nil
}

func (outdatedModPlacementReaderFixture) InstallationModInventory(_ context.Context, targetID, installationID string) (mods.RuntimeInstallationModInventory, error) {
	return mods.RuntimeInstallationModInventory{
		TargetID: targetID, InstallationID: installationID,
		Items: []mods.RuntimeInstallationMod{
			{
				SteamMod:       mods.SteamMod{ID: "378160973", Name: "Global Positions", Dependencies: []string{}, Tags: []string{}},
				CurrentVersion: "1.7.5", LatestVersion: "1.7.6", VersionStatus: "outdated", FileStatus: "ready",
			},
			{
				SteamMod:       mods.SteamMod{ID: "376333686", Name: "Combined Status", Dependencies: []string{}, Tags: []string{}},
				CurrentVersion: "1.9.8", LatestVersion: "1.9.8", VersionStatus: "current", FileStatus: "ready",
			},
		},
		Total: 2, Current: 1, Outdated: 1, ObservedAt: time.Now().UTC(),
	}, nil
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
	response = performJSON(router, http.MethodGet, "/api/v2/runtime-targets/local/installations/default/mods", nil, nil, "")
	assertAPIError(t, response, http.StatusServiceUnavailable, "MOD_RUNTIME_INVENTORY_UNAVAILABLE")
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

func TestModHTTPReadsSelectedRuntimeInstallationInventory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewModHandler(modHandlerService{}, nil)
	handler.ConfigurePlacementReader(modPlacementReaderFixture{})
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodGet, "/api/v2/runtime-targets/agent:node/installations/native/mods", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	data := responseData(t, response)
	if data["targetId"] != "agent:node" || data["installationId"] != "native" || data["total"] != float64(1) || data["current"] != float64(1) {
		t.Fatalf("unexpected runtime inventory response: %s", response.Body.String())
	}
	items, ok := data["items"].([]interface{})
	if !ok || len(items) != 1 || items[0].(map[string]interface{})["id"] != "378160973" {
		t.Fatalf("unexpected runtime inventory items: %#v", data["items"])
	}
}

func TestModHTTPPreservesRuntimeInventoryFailureReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewModHandler(modHandlerService{}, nil)
	handler.ConfigurePlacementReader(failingModPlacementReaderFixture{})
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodGet, "/api/v2/runtime-targets/agent:offline/installations/native/mods", nil, nil, "")
	assertAPIError(t, response, http.StatusServiceUnavailable, "MOD_RUNTIME_INVENTORY_FAILED")
	if !strings.Contains(response.Body.String(), `"reason":"运行机器离线"`) {
		t.Fatalf("runtime inventory failure lost its reason: %s", response.Body.String())
	}
}

func TestModHTTPUpdatesRuntimeInstallationContentThroughJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "mods_installation_update_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	updater := &modInstallationUpdaterFixture{}
	refresher := &modInstallationRefresherFixture{}
	handler := NewModHandler(modHandlerService{}, jobService)
	handler.ConfigurePlacementReader(outdatedModPlacementReaderFixture{})
	handler.ConfigureInstallationUpdater(updater)
	handler.ConfigureInstallationUpdateRefresher(refresher)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodPost, "/api/v2/runtime-targets/agent:node/installations/native/mods/378160973/actions/update", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "mod.installation.update" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected single installation update job: %#v", job)
	}
	assertModJobProgressEvent(t, jobService, job.ID, 37, "正在下载机器模组")
	assertModJobTransferEvent(t, jobService, job.ID, 32<<20, 92<<20, 4_500_375)
	calls, targetID, installationID, modIDs := updater.snapshot()
	if calls != 1 || targetID != "agent:node" || installationID != "native" || len(modIDs) != 1 || modIDs[0] != "378160973" {
		t.Fatalf("unexpected single installation update call: calls=%d target=%q installation=%q mods=%v", calls, targetID, installationID, modIDs)
	}
	if calls, targetID, installationID := refresher.snapshot(); calls != 1 || targetID != "agent:node" || installationID != "native" {
		t.Fatalf("unexpected single installation refresh: calls=%d target=%q installation=%q", calls, targetID, installationID)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/runtime-targets/agent:node/installations/native/mods/actions/update-outdated", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job = waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "mod.installation.update-all" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected batch installation update job: %#v", job)
	}
	calls, targetID, installationID, modIDs = updater.snapshot()
	if calls != 2 || targetID != "agent:node" || installationID != "native" || len(modIDs) != 1 || modIDs[0] != "378160973" {
		t.Fatalf("unexpected batch installation update call: calls=%d target=%q installation=%q mods=%v", calls, targetID, installationID, modIDs)
	}
	if calls, targetID, installationID := refresher.snapshot(); calls != 2 || targetID != "agent:node" || installationID != "native" {
		t.Fatalf("unexpected batch installation refresh: calls=%d target=%q installation=%q", calls, targetID, installationID)
	}
}

func TestModHTTPReportsInstallationRefreshFailureAfterContentUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "mods_installation_refresh_failure_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	updater := &modInstallationUpdaterFixture{}
	refresher := &modInstallationRefresherFixture{err: errors.New("运行机器状态读取失败")}
	handler := NewModHandler(modHandlerService{}, jobService)
	handler.ConfigurePlacementReader(outdatedModPlacementReaderFixture{})
	handler.ConfigureInstallationUpdater(updater)
	handler.ConfigureInstallationUpdateRefresher(refresher)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodPost, "/api/v2/runtime-targets/agent:node/installations/native/mods/378160973/actions/update", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Status != jobs.StatusSucceeded || job.Outcome != jobs.OutcomeFull || job.Error != nil ||
		len(job.Targets) != 1 || job.Targets[0].Warning == nil ||
		!strings.Contains(job.Targets[0].Warning.Message, "关联房间的更新检查未启动") {
		t.Fatalf("refresh failure job = %#v", job)
	}
	if calls, _, _, _ := updater.snapshot(); calls != 1 {
		t.Fatalf("content update calls = %d", calls)
	}
}

func TestModHTTPDownloadsDirectlyToSelectedInstallation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "mods_installation_download_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	updater := &modInstallationUpdaterFixture{}
	refresher := &modInstallationRefresherFixture{}
	handler := NewModHandler(modHandlerService{}, jobService)
	handler.ConfigurePlacementReader(modPlacementReaderFixture{})
	handler.ConfigureInstallationUpdater(updater)
	handler.ConfigureInstallationUpdateRefresher(refresher)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodPost, "/api/v2/runtime-targets/agent:node/installations/native/mods/3755481449/actions/download", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "mod.installation.download" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("download job=%#v", job)
	}
	if calls, targetID, installationID, modIDs := updater.snapshot(); calls != 1 || targetID != "agent:node" || installationID != "native" || len(modIDs) != 1 || modIDs[0] != "3755481449" {
		t.Fatalf("download call: calls=%d target=%q installation=%q mods=%v", calls, targetID, installationID, modIDs)
	}
	if calls, _, _ := refresher.snapshot(); calls != 0 {
		t.Fatalf("download refreshed installation update state %d times", calls)
	}
}

func TestModHTTPEnableUsesSynchronousPlacementWriter(t *testing.T) {
	fixture := &directModEnabledFixture{}
	handler := NewModHandler(modHandlerService{}, nil)
	handler.ConfigurePlacementReader(fixture)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room/mods/378160973/actions/enable", map[string]interface{}{
		"worldIds": []string{"world"}, "enabled": false, "expectedRevision": "current",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	data := responseData(t, response)
	if data["revision"] != "next" || fixture.request.ExpectedRevision != "current" || fixture.request.Enabled {
		t.Fatalf("response=%#v request=%#v", data, fixture.request)
	}
}

func assertModJobProgressEvent(t *testing.T, service *jobs.Service, jobID string, progress int, message string) {
	t.Helper()
	events, err := service.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.JobID == jobID && event.Type == "job.progress" && event.Data.Progress == progress && event.Data.Message == message {
			return
		}
	}
	t.Fatalf("job %q did not emit progress %d with message %q: %#v", jobID, progress, message, events)
}

func assertModJobTransferEvent(t *testing.T, service *jobs.Service, jobID string, currentBytes, totalBytes, bytesPerSecond int64) {
	t.Helper()
	events, err := service.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		transfer := event.Data.Transfer
		if event.JobID == jobID && event.Type == "job.progress" && transfer != nil &&
			transfer.CurrentBytes == currentBytes && transfer.TotalBytes == totalBytes && transfer.BytesPerSecond == bytesPerSecond {
			return
		}
	}
	t.Fatalf("job %q did not emit transfer %d/%d at %d B/s: %#v", jobID, currentBytes, totalBytes, bytesPerSecond, events)
}

func TestModJobProgressMapsOperationStages(t *testing.T) {
	tests := []struct {
		stage   string
		percent int
		want    int
	}{
		{operationprogress.StageModInspect, 0, 5},
		{operationprogress.StageModInspect, 100, 10},
		{operationprogress.StageModCache, 50, 37},
		{operationprogress.StageModPrepare, 100, 80},
		{operationprogress.StageModPublish, 100, 95},
		{operationprogress.StageModComplete, 100, 99},
		{operationprogress.StageModDone, 100, 99},
	}
	for _, test := range tests {
		if got := modJobProgress(operationprogress.Update{Stage: test.stage, Percent: test.percent}); got != test.want {
			t.Fatalf("modJobProgress(%q, %d) = %d, want %d", test.stage, test.percent, got, test.want)
		}
	}
}

func TestModHTTPRuntimeInstallationUpdateNoopAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "mods_installation_noop_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	updater := &modInstallationUpdaterFixture{}
	handler := NewModHandler(modHandlerService{}, jobService)
	handler.ConfigurePlacementReader(modPlacementReaderFixture{})
	handler.ConfigureInstallationUpdater(updater)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodPost, "/api/v2/runtime-targets/local/installations/default/mods/actions/update-outdated", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "mod.installation.update-all" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected no-op installation update job: %#v", job)
	}
	if calls, _, _, _ := updater.snapshot(); calls != 0 {
		t.Fatalf("no-op update called updater %d times", calls)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/runtime-targets/local/installations/default/mods/not-a-workshop-id/actions/update", nil, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_MOD_ID")

	unavailable := NewModHandler(modHandlerService{}, jobService)
	unavailable.ConfigurePlacementReader(modPlacementReaderFixture{})
	unavailableRouter := gin.New()
	unavailable.Register(unavailableRouter.Group("/api/v2"))
	response = performJSON(unavailableRouter, http.MethodPost, "/api/v2/runtime-targets/local/installations/default/mods/378160973/actions/update", nil, nil, "")
	assertAPIError(t, response, http.StatusServiceUnavailable, "MOD_INSTALLATION_UPDATE_UNAVAILABLE")
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

func TestModJobErrorPreservesSteamCMDReason(t *testing.T) {
	err := fmt.Errorf("%w: I/O Operation Failed", mods.ErrSteamCMDDownload)
	jobErr := modJobError(err)
	if jobErr.Code != "STEAMCMD_DOWNLOAD_FAILED" || !strings.Contains(jobErr.Message, "I/O Operation Failed") {
		t.Fatalf("Mod job error = %#v", jobErr)
	}
}

func TestModPlacementConflictUsesHTTPAndJobConflictCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	failure := fmt.Errorf("Mod placement changed: %w", runtimedriver.ErrTopologyChanged)
	router := gin.New()
	router.GET("/mods", func(c *gin.Context) { modFailure(c, failure) })
	response := performJSON(router, http.MethodGet, "/mods", nil, nil, "")
	assertAPIError(t, response, http.StatusConflict, "TOPOLOGY_CHANGED")
	if jobErr := modJobError(failure); jobErr.Code != "TOPOLOGY_CHANGED" || !strings.Contains(jobErr.Message, "刷新") {
		t.Fatalf("unexpected job error: %#v", jobErr)
	}
}

func TestModHTTPUnknownFailurePreservesOperationReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	failure := errors.New("inspect Workshop content: I/O Operation Failed")
	router := gin.New()
	router.Use(RequestContext())
	router.GET("/mods", func(c *gin.Context) { modFailure(c, failure) })

	response := performJSON(router, http.MethodGet, "/mods", nil, nil, "")
	assertAPIError(t, response, http.StatusInternalServerError, "MOD_OPERATION_FAILED")
	if !strings.Contains(response.Body.String(), `"reason":"inspect Workshop content: I/O Operation Failed"`) {
		t.Fatalf("Mod operation failure lost its reason: %s", response.Body.String())
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("Mod operation failure did not include a request ID")
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
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["revision"] != "next" {
		t.Fatalf("unexpected configuration result: %s", response.Body.String())
	}
}

func TestPlacementAwareModHandlerKeepsLightweightLocalMutationsAvailable(t *testing.T) {
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
		{"/api/v2/rooms/room/mods/actions/install", map[string]interface{}{}},
		{"/api/v2/rooms/room/mods/378160973/actions/update", nil},
		{"/api/v2/rooms/room/mods/378160973/actions/repair", map[string]interface{}{}},
		{"/api/v2/rooms/room/mods/378160973/actions/uninstall", map[string]interface{}{}},
	}
	for _, item := range requests {
		response := performJSON(router, http.MethodPost, item.path, item.body, nil, "")
		assertAPIError(t, response, http.StatusConflict, "MOD_PUBLICATION_REQUIRED")
	}
	for _, item := range []struct {
		path string
		body interface{}
	}{
		{"/api/v2/rooms/room/mods/378160973/actions/add", map[string]interface{}{"worldIds": []string{"world"}, "enabled": true}},
		{"/api/v2/rooms/room/mods/378160973/actions/enable", map[string]interface{}{"worldIds": []string{"world"}, "enabled": false}},
	} {
		response := performJSON(router, http.MethodPost, item.path, item.body, nil, "")
		assertStatus(t, response, http.StatusAccepted)
		job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
		if job.Outcome != jobs.OutcomeFull {
			t.Fatalf("unexpected local Mod job: %#v", job)
		}
	}

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room/worlds/world/mods/378160973/configuration/actions/apply", map[string]interface{}{
		"expectedRevision": "current", "worldIds": []string{"world", "caves"}, "enabled": true, "patch": map[string]interface{}{},
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["publishedTargets"] != float64(2) {
		t.Fatalf("unexpected placement configuration result: %s", response.Body.String())
	}

	response = performJSON(router, http.MethodPost, "/api/v2/mods/library/378160973/actions/update", nil, nil, "")
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
