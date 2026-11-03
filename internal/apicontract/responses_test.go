package apicontract

import (
	"encoding/json"
	"testing"
	"time"

	"dont/internal/gameinstall"
	"dont/internal/luajit"
	"dont/internal/mods"
	"dont/internal/modupdates"
	"dont/shared"
)

func TestInstallationAndSteamSchemasMatchSerializedPlatformTypes(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	login := shared.SteamLoginStatus{Mode: "anonymous", State: "anonymous", UpdatedAt: time.Now().UTC()}
	game := shared.GameInstallationReport{ServerPath: "/server", SavePath: "/saves", SteamLogin: &login}
	for _, sample := range []struct {
		operation, status string
		value             any
	}{
		{"getRoomModUpdate", "200", modupdates.Overview{Policy: modupdates.DefaultPolicy("example"), State: modupdates.State{RoomID: "example", Status: modupdates.StatusAvailable, AvailableModIDs: []string{"100"}, UpdatedAt: time.Now().UTC(), CheckSummary: &mods.ModUpdateCheck{CheckedAt: time.Now().UTC(), Scope: mods.ModUpdateScope{Revision: "revision", AffectedRoomIDs: []string{"example"}}, Total: 2, Outdated: 1, Unknown: 1, Items: []mods.ModUpdateCheckItem{{ModID: "100", Status: "outdated", Targets: []mods.RuntimeModVersionTarget{{TargetID: "local", InstallationID: "default", Status: "outdated", Version: "1"}}}}}, PendingUpdate: &modupdates.PendingUpdate{ModIDs: []string{"100"}, AffectedRoomIDs: []string{"example"}, Targets: []mods.ModUpdateTarget{{TargetID: "local", InstallationID: "default", ModIDs: []string{"100"}, Status: "verified"}}, RestartedRoomIDs: []string{}}}}},
		{"listGameInstallations", "200", []gameinstall.Installation{{TargetID: "local", InstallationID: "default", GameInstallationReport: game}}},
		{"probeExistingGameInstallation", "200", game},
		{"observeNodeSteamLogin", "200", login},
		{"manageNodeSteamLogin", "200", login},
		{"listLuaJITPackagesAndInstallations", "200", luajit.Catalog{Transfers: []shared.LuaJITRelease{}, Installations: []luajit.Installation{{TargetID: "local", InstallationID: "default"}}}},
		{"inspectLuaJITInstallation", "200", luajit.Installation{TargetID: "local", InstallationID: "default"}},
		{"uploadLuaJITPackage", "201", shared.LuaJITRelease{ID: "release", Version: "1", OS: "linux", Arch: "amd64", SHA256: "digest", Size: 12}},
		{"setRoomModConfigurationMode", "200", map[string]any{"mode": "separate"}},
	} {
		t.Run(sample.operation, func(t *testing.T) {
			op, _ := c.Operation(sample.operation)
			response := op.Responses[sample.status]
			fake := &Operation{ID: "response", Method: "GET", catalog: c, RequestBody: Body{Required: true, Content: map[string]Media{"application/json": {Schema: response.Content["application/json"].Schema}}}}
			raw, _ := json.Marshal(map[string]any{"body": map[string]any{"data": sample.value, "error": nil, "meta": map[string]any{"requestId": "sample"}}})
			if _, err := fake.Validate(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}
