package httpapi

import (
	"context"
	"dont/internal/agents"
	"dont/internal/gameinstall"
	"dont/internal/gameupdate"
	"dont/shared"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
	"testing"
)

type maintenanceTargets struct {
	report  shared.GameInstallationReport
	offline bool
	calls   []shared.RuntimeOperationRequest
}

func (f *maintenanceTargets) RuntimeTargets() ([]agents.RuntimeTarget, error) {
	return []agents.RuntimeTarget{{ID: "agent:worker", Online: !f.offline, Capabilities: []string{"runtime.game-install.v1"}, Installations: []agents.RuntimeInstallation{{ID: "secondary"}}}}, nil
}
func (f *maintenanceTargets) ExecuteRuntime(_ context.Context, target string, r shared.RuntimeOperationRequest, _ int) (agents.RuntimeExecutionResult, error) {
	if target != "agent:worker" || r.InstallationID != "secondary" || r.Action != shared.RuntimeActionGameInstallationObserve || r.GameInstallation == nil || !r.GameInstallation.CheckUsage {
		return agents.RuntimeExecutionResult{}, errors.New("unexpected mutation or route")
	}
	f.calls = append(f.calls, r)
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{GameInstallation: &f.report}}, nil
}
func TestInstallationMaintenancePreflight(t *testing.T) {
	for _, scenario := range []string{"managed-running", "unmanaged", "old-agent", "offline", "idle-unused", "busy-unused", "new-install", "planner-failure", "connected-offline"} {
		t.Run(scenario, func(t *testing.T) {
			node := &maintenanceTargets{report: shared.GameInstallationReport{Installed: true, CanInstall: true, UsageState: "running", RunningProcesses: 1}}
			release := &gameReleaseHTTPFixture{plan: gameupdate.ReleasePlan{Ready: true, Policy: gameupdate.ReleasePolicy{Validate: true}, Installations: []gameupdate.ReleaseInstallationPlan{{TargetID: "agent:worker", InstallationID: "secondary", RunningShards: 1}}}}
			wantReady, wantMode, wantStatus := true, "release", http.StatusOK
			switch scenario {
			case "unmanaged":
				node.report.RunningProcesses = 2
				wantReady = false
			case "old-agent":
				node.report.UsageState = ""
				wantReady = false
				wantMode = "install"
			case "offline":
				node.offline = true
				wantStatus = http.StatusUnprocessableEntity
			case "idle-unused":
				node.report.UsageState = "idle"
				node.report.RunningProcesses = 0
				release.previewErr = gameupdate.ErrReleaseInstallationUnused
				wantMode = "install"
			case "busy-unused":
				release.previewErr = gameupdate.ErrReleaseInstallationUnused
				wantReady = false
				wantMode = "install"
			case "new-install":
				node.report.Installed = false
				node.report.UsageState = "idle"
				node.report.RunningProcesses = 0
				wantMode = "install"
			case "connected-offline":
				release.plan.Installations = append(release.plan.Installations, gameupdate.ReleaseInstallationPlan{TargetID: "agent:other", InstallationID: "default"})
				wantReady = false
			case "planner-failure":
				release.previewErr = gameupdate.ErrLatestBuildUnavailable
				wantStatus = http.StatusServiceUnavailable
			}
			handler := NewGameInstallationHandler(gameinstall.NewService(node, nil, nil))
			handler.ConfigureReleases(release)
			router := gin.New()
			handler.Register(router.Group("/api/v2"))
			response := performJSON(router, http.MethodPost, "/api/v2/runtime-targets/game-installations/preview", map[string]interface{}{"targetId": "agent:worker", "installationId": "secondary", "restartRunning": false}, nil, "")
			assertStatus(t, response, wantStatus)
			if wantStatus != http.StatusOK {
				return
			}
			var result struct{ Data gameInstallationPreview }
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Data.Ready != wantReady || result.Data.Mode != wantMode {
				t.Fatalf("preflight=%s", response.Body.String())
			}
			if !wantReady && len(result.Data.Blockers) == 0 {
				t.Fatal("missing actionable blocker")
			}
			if scenario == "managed-running" {
				r := release.previewRequest
				if len(r.Installations) != 1 || r.Installations[0].InstallationID != "secondary" || r.Installations[0].TargetID != "agent:worker" || !r.Policy.Validate || r.Policy.RestartRunning == nil || *r.Policy.RestartRunning {
					t.Fatalf("wrong release preview: %+v", r)
				}
			}
			if len(node.calls) != 1 {
				t.Fatal("preflight did extra work", node.calls)
			}
		})
	}
}
