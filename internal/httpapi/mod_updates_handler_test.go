package httpapi

import (
	"net/http"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/modupdates"

	"github.com/gin-gonic/gin"
)

type modUpdateHTTPStub struct {
	applyNowRoomID string
}

func (s *modUpdateHTTPStub) Overview(roomID string) (modupdates.Overview, error) {
	return modupdates.Overview{Policy: modupdates.DefaultPolicy(roomID), State: modupdates.DefaultState(roomID, time.Now())}, nil
}

func (s *modUpdateHTTPStub) UpdatePolicy(roomID string, _ modupdates.PolicyInput) (modupdates.Overview, error) {
	return s.Overview(roomID)
}

func (s *modUpdateHTTPStub) SubmitCheck(roomID string) (jobs.Job, error) {
	return queuedModUpdateHTTPJob(roomID, "mod.update.check"), nil
}

func (s *modUpdateHTTPStub) SubmitApplyWhenEmpty(roomID string) (jobs.Job, error) {
	return queuedModUpdateHTTPJob(roomID, "mod.update.activate"), nil
}

func (s *modUpdateHTTPStub) SubmitApplyNow(roomID string) (jobs.Job, error) {
	s.applyNowRoomID = roomID
	return queuedModUpdateHTTPJob(roomID, "mod.update.activate"), nil
}

func queuedModUpdateHTTPJob(roomID, kind string) jobs.Job {
	return jobs.Job{
		ID: "job", Kind: kind, RoomID: roomID, Status: jobs.StatusQueued, Outcome: jobs.OutcomePending,
		CreatedAt: time.Now().UTC(), Targets: []jobs.Target{},
	}
}

func TestModUpdateApplyNowHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &modUpdateHTTPStub{}
	handler, err := NewModUpdateHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room-1/mod-update/actions/apply", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	if service.applyNowRoomID != "room-1" {
		t.Fatalf("manual apply room = %q", service.applyNowRoomID)
	}
	data := responseData(t, response)
	if data["kind"] != "mod.update.activate" || data["status"] != string(jobs.StatusQueued) {
		t.Fatalf("unexpected manual apply response: %s", response.Body.String())
	}
}

type checkedModUpdateHTTPStub struct {
	modUpdateHTTPStub
	request modupdates.ApplyRequest
}

func (s *checkedModUpdateHTTPStub) SubmitApplyNowChecked(id string, request modupdates.ApplyRequest) (jobs.Job, error) {
	s.request = request
	return s.SubmitApplyNow(id)
}
func TestModUpdateConfirmationBodyIsPreservedAndUnknownFieldsRejected(t *testing.T) {
	service := &checkedModUpdateHTTPStub{}
	handler, _ := NewModUpdateHandler(service)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room-1/mod-update/actions/apply", map[string]any{"expectedScopeRevision": "scope-v1", "modIds": []string{"100"}}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	if service.request.ExpectedScopeRevision != "scope-v1" || len(service.request.ModIDs) != 1 || service.request.ModIDs[0] != "100" {
		t.Fatal(service.request)
	}
	service.applyNowRoomID = ""
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room-1/mod-update/actions/apply", map[string]any{"force": true}, nil, "")
	if response.Code < 400 || service.applyNowRoomID != "" {
		t.Fatal(response.Body.String())
	}
}
