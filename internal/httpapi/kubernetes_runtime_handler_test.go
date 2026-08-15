package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"dont/internal/kubernetesruntime"

	"github.com/gin-gonic/gin"
)

type kubernetesRuntimeHTTPFixture struct {
	view        kubernetesruntime.ServiceView
	observation kubernetesruntime.Observation
	preview     kubernetesruntime.Preview
	err         error
	providerID  string
}

func (fixture *kubernetesRuntimeHTTPFixture) Status() kubernetesruntime.ServiceView {
	return fixture.view
}
func (fixture *kubernetesRuntimeHTTPFixture) Observe(_ context.Context, providerID, _, _ string) (kubernetesruntime.Observation, error) {
	fixture.providerID = providerID
	return fixture.observation, fixture.err
}
func (fixture *kubernetesRuntimeHTTPFixture) Preview(_ context.Context, providerID string, _ kubernetesruntime.Request) (kubernetesruntime.Preview, error) {
	fixture.providerID = providerID
	return fixture.preview, fixture.err
}

func TestKubernetesRuntimeHTTPExposesStatusObserveAndPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &kubernetesRuntimeHTTPFixture{
		view:        kubernetesruntime.ServiceView{Release: kubernetesruntime.ReleaseExperimental, Enabled: true, Configured: true, Status: kubernetesruntime.ServiceAvailable},
		observation: kubernetesruntime.Observation{Ref: kubernetesruntime.ShardRef{ProviderID: "home", RoomID: "room", WorldID: "world"}},
		preview:     kubernetesruntime.Preview{Preflight: kubernetesruntime.PreflightReport{Release: kubernetesruntime.ReleaseExperimental, Ready: false}},
	}
	router := gin.New()
	handler, err := NewKubernetesRuntimeHandler(fixture)
	if err != nil {
		t.Fatal(err)
	}
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodGet, "/api/v2/runtime-providers/kubernetes", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodPost, "/api/v2/runtime-providers/kubernetes/home/shards/observe", map[string]string{"roomId": "room", "worldId": "world"}, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodPost, "/api/v2/runtime-providers/kubernetes/home/shards/preflight", map[string]interface{}{"action": "start", "ref": map[string]string{"roomId": "room", "worldId": "world"}}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if fixture.providerID != "home" {
		t.Fatalf("provider id = %q", fixture.providerID)
	}
}

func TestKubernetesRuntimeHTTPReportsDisabledAndUnavailableStates(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "disabled", err: kubernetesruntime.ErrDisabled, status: http.StatusConflict, code: "KUBERNETES_EXPERIMENT_DISABLED"},
		{name: "unavailable", err: kubernetesruntime.ErrUnavailable, status: http.StatusServiceUnavailable, code: "KUBERNETES_PROVIDER_UNAVAILABLE"},
		{name: "missing", err: kubernetesruntime.ErrProviderMissing, status: http.StatusNotFound, code: "KUBERNETES_PROVIDER_NOT_FOUND"},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			handler, _ := NewKubernetesRuntimeHandler(&kubernetesRuntimeHTTPFixture{err: test.err})
			handler.Register(router.Group("/api/v2"))
			response := performJSON(router, http.MethodPost, "/api/v2/runtime-providers/kubernetes/home/shards/observe", map[string]string{"roomId": "room", "worldId": "world"}, nil, "")
			assertStatus(t, response, test.status)
			if body := response.Body.String(); !strings.Contains(body, `"code":"`+test.code+`"`) {
				t.Fatalf("response body = %s", body)
			}
		})
	}
}

func TestKubernetesRuntimeHTTPRejectsIncompleteObservationInput(t *testing.T) {
	router := gin.New()
	fixture := &kubernetesRuntimeHTTPFixture{}
	handler, _ := NewKubernetesRuntimeHandler(fixture)
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodPost, "/api/v2/runtime-providers/kubernetes/home/shards/observe", map[string]string{"roomId": "room"}, nil, "")
	assertStatus(t, response, http.StatusBadRequest)
	if fixture.providerID != "" {
		t.Fatalf("invalid input reached service with provider %q", fixture.providerID)
	}
}
