package apicontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShippedDescriptionsAndReplacements(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range c.Operations() {
		if op.Callable() && (len(strings.TrimSpace(op.Summary)) < 12 || strings.HasPrefix(op.Summary, "MCP tool ")) {
			t.Errorf("missing meaningful summary: %s", op.ID)
		}
		if op.ReplacedBy != "" {
			replacement, ok := c.Operation(op.ReplacedBy)
			if !ok || replacement.Deprecated {
				t.Errorf("invalid replacement: %s", op.ID)
			}
		}
	}
	for _, id := range []string{"listRoomCommands", "refreshWorldStates", "updateMod", "repairMod", "uninstallMod"} {
		op, _ := c.Operation(id)
		if !op.Deprecated {
			t.Errorf("lost deprecation: %s", id)
		}
		if got := c.Search(id, 1); len(got) != 1 || got[0].ID != id {
			t.Errorf("exact deprecated ID not discoverable: %s", id)
		}
	}
}

func TestParameterDescriptionReachesToolSchema(t *testing.T) {
	c, err := Parse([]byte(`openapi: 3.1.0
paths:
  /sample:
    get:
      operationId: sample
      parameters:
        - name: limit
          in: query
          description: Maximum number of returned observations
          schema: {type: integer, description: less specific}
      responses: {"200": {description: OK}}
`))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := c.Operation("sample")
	schema, err := op.AIInputSchema()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(schema)
	if !strings.Contains(string(encoded), "Maximum number of returned observations") {
		t.Fatal(string(encoded))
	}
	if op.Parameters[0].Schema["description"] != "less specific" {
		t.Fatal("shared parameter schema was mutated")
	}
}

func TestDiscoveryDistinguishesOverlappingOperations(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct{ query, want string }{
		{"global command definitions", "listCommands"},
		{"list command definitions", "listCommands"},
		{"listCommands", "listCommands"},
		{"download Workshop mod explicit Runtime installation", "downloadRuntimeInstallationMod"},
		{"download Runtime installation mod", "downloadRuntimeInstallationMod"},
		{"downloadRuntimeInstallationMod", "downloadRuntimeInstallationMod"},
		{"enable automatic room backups", "configureRoomBackupSchedule"},
		{"pause automatic backups schedule", "configureRoomBackupSchedule"},
		{"automatic backup tasks group state defaults", "listRoomBackupSchedules"},
		{"latest room world states", "listWorldStates"},
		{"read current world state snapshots", "listWorldStates"},
		{"listWorldStates", "listWorldStates"},
		{"check room mod update workflow", "checkRoomModUpdate"},
		{"compare mod versions without workflow", "checkModUpdates"},
		{"checkRoomModUpdate", "checkRoomModUpdate"},
		{"update one Workshop mod explicit installation", "updateRuntimeInstallationMod"},
		{"update outdated Workshop content installation", "updateOutdatedRuntimeInstallationMods"},
		{"apply room mod update now restart", "applyRoomModUpdateNow"},
		{"command history across Agents", "listAgentCommands"},
		{"command history one specific Agent", "listAgentCommandsForAgent"},
		{"listAgentCommandsForAgent", "listAgentCommandsForAgent"},
		{"installed game versions managed Runtime", "getInstalledGameVersions"},
		{"latest official Steam Klei game version", "getGameVersion"},
		{"getInstalledGameVersions", "getInstalledGameVersions"},
	}
	for _, f := range fixtures {
		t.Run(f.query, func(t *testing.T) {
			candidates := c.Search(f.query, 3)
			ids := []string{}
			found := false
			for _, op := range candidates {
				ids = append(ids, op.ID)
				found = found || op.ID == f.want
			}
			if !found {
				t.Fatalf("want %s in top 3, got %v", f.want, ids)
			}
		})
	}
	for _, pair := range [][2]string{{"listCommands", "listRoomCommands"}, {"listWorldStates", "refreshWorldStates"}} {
		query := "command definitions"
		if pair[0] == "listWorldStates" {
			query = "current world states"
		}
		ranks := map[string]int{}
		for i, op := range c.Search(query, 30) {
			ranks[op.ID] = i
		}
		preferred, ok1 := ranks[pair[0]]
		legacy, ok2 := ranks[pair[1]]
		if ok2 && (!ok1 || legacy < preferred) {
			t.Errorf("compatibility duplicate precedes recommendation: %s %+v", query, ranks)
		}
	}
}

func TestLocalModContractKeepsSteamActionsNumeric(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"getModConfiguration", "setModEnabled"} {
		op, _ := c.Operation(id)
		raw := `{"path":{"roomId":"room","worldId":"Master","modId":"local:鱼干"}}`
		if id == "setModEnabled" {
			raw = `{"path":{"roomId":"room","modId":"local:鱼干"},"body":{"worldIds":["Master"],"enabled":false,"expectedRevision":"revision"},"requestId":"local-toggle-1"}`
		}
		if _, err := op.Validate([]byte(raw)); err != nil {
			t.Errorf("%s rejected local ID: %v", id, err)
		}
	}
	op, _ := c.Operation("downloadRuntimeInstallationMod")
	if _, err := op.Validate([]byte(`{"path":{"targetId":"local","installationId":"default","modId":"local:鱼干"}}`)); err == nil {
		t.Error("local ID accepted for Steam download")
	}
}

func TestModReferenceSchemaAcceptsDirectoryNamesAndRejectsSeparators(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	op, _ := c.Operation("getModDetails")
	for _, id := range []string{"123", "local:123", "local:fishdried_custom", "fishdried_custom", "local:鱼干_自用", "local:workshop-custom", "local:xX_0-9", "local:MOD WITH SPACE", "123custom", "123-foo", "local:123custom"} {
		input, _ := json.Marshal(map[string]any{"path": map[string]string{"modId": id}, "query": map[string]string{"targetId": "local", "installationId": "default"}})
		if _, err := op.Validate(input); err != nil {
			t.Errorf("valid %s: %v", id, err)
		}
	}
	for _, id := range []string{"local:../escape", "local:bad\\name", "local:bad\x00name", "local:bad\nname"} {
		input, _ := json.Marshal(map[string]any{"path": map[string]string{"modId": id}})
		if _, err := op.Validate(input); err == nil {
			t.Errorf("unsafe reference %q accepted", id)
		}
	}
}
