package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"dont/internal/jobs"
	"dont/internal/saveimport"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type saveImportHandlerApp struct {
	router  *gin.Engine
	jobs    *jobs.Service
	imports *saveimport.Service
}

func newSaveImportHandlerApp(t *testing.T) saveImportHandlerApp {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	jobStore := jobs.NewStore(db, "save_import_http_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	importStore := saveimport.NewStore(db, "save_import_http_")
	if err := importStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := saveimport.NewService(saveimport.Config{
		SaveRoot: filepath.Join(root, "saves"), ImportRoot: filepath.Join(root, "imports"), WorkshopRoot: filepath.Join(root, "workshop"),
	}, importStore, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewSaveImportHandler(service, jobService).Register(v2)
	NewJobHandler(jobService).Register(v2)
	return saveImportHandlerApp{router: router, jobs: jobService, imports: service}
}

func TestSaveImportHTTPUploadsAndAnalyzesWrappedArchive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newSaveImportHandlerApp(t)
	archive := saveImportHTTPZip(t, map[string]string{
		"Cluster_1/cluster.ini":         "[NETWORK]\ncluster_name = HTTP Import\n[GAMEPLAY]\ngame_mode = survival\n",
		"Cluster_1/Master/server.ini":   "[NETWORK]\nserver_port = 10999\n[SHARD]\nis_master = true\nid = 1\n",
		"Cluster_1/Master/save/session": "save",
	})
	response := performSaveImportUpload(t, app.router, "import.zip", "HTTP Import", archive)
	assertStatus(t, response, http.StatusAccepted)
	var envelope struct {
		Data struct {
			Import saveimport.Session `json:"import"`
			Job    jobs.Job           `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Import.ID == "" || envelope.Data.Job.ID == "" {
		t.Fatalf("response = %s", response.Body.String())
	}
	waitForJobStatus(t, app.jobs, envelope.Data.Job.ID, jobs.StatusSucceeded)
	request := httptest.NewRequest(http.MethodGet, "/api/v2/save-imports/"+envelope.Data.Import.ID, nil)
	result := httptest.NewRecorder()
	app.router.ServeHTTP(result, request)
	assertStatus(t, result, http.StatusOK)
	data := responseData(t, result)
	if data["status"] != string(saveimport.StatusReady) {
		t.Fatalf("save import = %s", result.Body.String())
	}
	manifest, _ := data["manifest"].(map[string]interface{})
	candidates, _ := manifest["candidates"].([]interface{})
	if len(candidates) != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestSaveImportHTTPPersistsInvalidAnalysisResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newSaveImportHandlerApp(t)
	archive := saveImportHTTPZip(t, map[string]string{"../cluster.ini": "bad"})
	response := performSaveImportUpload(t, app.router, "bad.zip", "Bad Import", archive)
	assertStatus(t, response, http.StatusAccepted)
	var envelope struct {
		Data struct {
			Import saveimport.Session `json:"import"`
			Job    jobs.Job           `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	waitForJobStatus(t, app.jobs, envelope.Data.Job.ID, jobs.StatusFailed)
	value, err := app.imports.Get(envelope.Data.Import.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != saveimport.StatusInvalid || value.ErrorCode != "UNSAFE_ARCHIVE" {
		t.Fatalf("import = %#v", value)
	}
}

func performSaveImportUpload(t *testing.T, router http.Handler, filename, name string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("name", name); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v2/save-imports/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func saveImportHTTPZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
