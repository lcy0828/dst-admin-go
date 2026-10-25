package saveimport

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMixedLocalModImportPreservesConfigurationAndUnrelatedSave(t *testing.T) {
	app := newApplyTestApp(t)
	createManagedRoom(t, app, "Existing", "Existing Room", "target-token-1234567890", 10999, "original-save")
	content := `return {fishdried_custom={enabled=true,configuration_options={mode="easy",future={keep=42}}},["workshop-1392778117"]={enabled=true},operator_value={keep="unchanged"}}`
	archive := createZIP(t, []archiveTestEntry{
		{name: "Cluster_1/cluster.ini", content: clusterINI("Source")},
		{name: "Cluster_1/cluster_token.txt", content: "source-token-1234567890"},
		{name: "Cluster_1/Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: "Cluster_1/Master/save/session/source/0000000001", content: "imported-save"},
		{name: "Cluster_1/Master/save/shardindex", content: `return {session_id="source"}`},
		{name: "Cluster_1/Master/modoverrides.lua", content: content},
	})
	value, err := app.service.Upload(context.Background(), "Local Mods", "Cluster_1.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.service.Apply(context.Background(), value.ID, "job-mixed-local", ApplyRequest{CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "ImportedMixed", RoomName: "Mixed", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsInstallMissing})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.InstalledMods) != 1 || result.InstalledMods[0] != "1392778117" {
		t.Fatalf("local identity entered Steam installation: %+v", result.InstalledMods)
	}
	data, err := os.ReadFile(filepath.Join(app.saveRoot, "ImportedMixed", "Master", "modoverrides.lua"))
	if err != nil || string(data) != content {
		t.Fatalf("import changed mixed configuration: %q %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(app.saveRoot, "ImportedMixed", "Master", "save", "session", "source", "0000000001"))
	if err != nil || string(data) != "imported-save" {
		t.Fatalf("imported save changed: %q %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(app.saveRoot, "Existing", "Master", "save", "session", "old", "0000000001"))
	if err != nil || string(data) != "original-save" {
		t.Fatalf("unrelated save changed: %q %v", data, err)
	}
}
