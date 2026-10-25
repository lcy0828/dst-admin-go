package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/shared"
	"github.com/gin-gonic/gin"
)

type observedHealthArtifactRuntime struct{ bundle shared.RuntimeArtifactBundle }

func (*observedHealthArtifactRuntime) IsLocalPlacement(string, string) (bool, error) {
	return false, nil
}
func (*observedHealthArtifactRuntime) Status(context.Context, string, string) (shared.ShardRuntimeStatus, error) {
	return shared.ShardRuntimeStatus{State: "running", SessionExists: true}, nil
}
func (*observedHealthArtifactRuntime) SendID(context.Context, string, string, shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error) {
	return shared.RuntimeOperationResult{}, nil
}
func (r *observedHealthArtifactRuntime) ReadArtifacts(context.Context, string, string, shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	return r.bundle, nil
}

type unusedRuntimeConsole struct{}

func (unusedRuntimeConsole) Send(context.Context, string, string, string) error { return nil }

func TestRemoteRuntimeStatusObservesPreviousProducerThroughActualBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	manager.statuses = manager.statuses[:1]
	process.running["Cluster_1/Master"] = true
	localManager, err := dstruntime.NewManager(t.TempDir(), observationMaintenanceCatalog{catalog})
	if err != nil {
		t.Fatal(err)
	}
	local, err := dstruntime.NewBridge(localManager, process, unusedRuntimeConsole{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	data, err := json.Marshal(dstruntime.Health{SchemaVersion: 1, ProducerVersion: "2.4.11", ProducerInstanceID: "published-runtime", SessionID: "SESSION", ShardID: "1", Sequence: 1, Running: true, Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	transport := &observedHealthArtifactRuntime{bundle: shared.RuntimeArtifactBundle{Kind: shared.ArtifactRuntimeHealth, Artifacts: []shared.RuntimeArtifact{{Name: "health.json", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), UpdatedAt: now, Data: data}}}}
	bridge, err := dstruntime.NewDistributedBridge(local, transport)
	if err != nil {
		t.Fatal(err)
	}
	updater := &handlerRuntimeUpdater{report: &dstruntime.UpdateReport{State: "updated", InstallationState: "installed", Version: dstruntime.RuntimeVersion, UpdatedAt: now}}
	handler := NewDSTRuntimeHandler(manager, catalog, process, bridge)
	handler.ConfigureUpdater(updater)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	item := responseData(t, response)["items"].([]interface{})[0].(map[string]interface{})
	if item["health"] == nil || item["healthState"] != "degraded" || item["version"] != dstruntime.RuntimeVersion || item["health"].(map[string]interface{})["producerVersion"] != "2.4.11" {
		t.Fatalf("actual DistributedBridge discards the active old producer before handler's version mismatch branch: %s", response.Body.String())
	}
}
