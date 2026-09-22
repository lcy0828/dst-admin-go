package agents

import (
	"errors"
	"reflect"
	"testing"
)

func TestDisplayAddressDoesNotChangeRuntimeIdentityOrPaths(t *testing.T) {
	service, store, _, _ := newAgentTestService(t)
	root := t.TempDir()
	service.ConfigureLocalRuntime(RuntimeConfig{SavePath: root, ServerPath: root, InstallationID: "default"})
	for _, id := range []string{"local", "agent:agent-primary", "agent:agent-offline"} {
		before, err := service.resolvePresentationTarget(id)
		if err != nil {
			t.Fatal(err)
		}
		after, err := service.SetRuntimeTargetDisplayAddress(id, "192.168.5.42")
		if err != nil {
			t.Fatal(err)
		}
		if after.ID != before.ID || !reflect.DeepEqual(after.Config, before.Config) || !reflect.DeepEqual(after.IPAddresses, before.IPAddresses) {
			t.Fatalf("display update changed runtime: %#v", after)
		}
		renamed, err := service.RenameRuntimeTarget(id, "Game host")
		if err != nil {
			t.Fatal(err)
		}
		if renamed.DisplayAddress != "192.168.5.42" || !renamed.DisplayNameCustom {
			t.Fatalf("rename lost presentation: %#v", renamed)
		}
		cleared, err := service.SetRuntimeTargetDisplayAddress(id, "")
		if err != nil {
			t.Fatal(err)
		}
		if cleared.Name != "Game host" || cleared.DisplayAddress != "" {
			t.Fatalf("clear lost name: %#v", cleared)
		}
	}
	// Migration is additive and preserves existing names and addresses.
	if _, err := service.SetRuntimeTargetDisplayAddress("local", "games.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	targets, err := service.RuntimeTargets()
	if err != nil {
		t.Fatal(err)
	}
	if targets[0].DisplayAddress != "games.example.com" || targets[0].Name != "Game host" {
		t.Fatalf("lost stored metadata: %#v", targets[0])
	}
	if _, err := service.SetRuntimeTargetDisplayAddress("agent:missing", "games.example.com"); !errors.Is(err, ErrRuntimeTargetNotFound) {
		t.Fatalf("unknown target: %v", err)
	}
}

func TestDisplayAddressValidation(t *testing.T) {
	for _, value := range []string{"", "192.168.1.2", "2001:db8::2", "dst.example.com", "game-host"} {
		if !validDisplayAddress(value) {
			t.Errorf("valid address rejected: %q", value)
		}
	}
	for _, value := range []string{"0.0.0.0", "::", "127.0.0.1", "::1", "localhost", "https://dst.example.com", "host:8081", "999.2.3.4", "-host", "host..com", "host/name", "host\nname"} {
		if validDisplayAddress(value) {
			t.Errorf("invalid address accepted: %q", value)
		}
	}
}

func TestPresentationMigrationPreservesLegacyMachineNames(t *testing.T) {
	_, store, _, _ := newAgentTestService(t)
	store.nodeNamesTable = "legacy_machine_names"
	if err := store.db.Exec(`CREATE TABLE legacy_machine_names (
		target_id varchar(160) PRIMARY KEY, display_name varchar(100) NOT NULL,
		created_at datetime, updated_at datetime
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.db.Exec("INSERT INTO legacy_machine_names (target_id, display_name) VALUES (?, ?)", "local", "Existing name").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveNodeDisplayAddress("local", "192.168.5.42"); err != nil {
		t.Fatal(err)
	}
	values, err := store.nodePresentations()
	if err != nil || values["local"].DisplayName != "Existing name" || values["local"].DisplayAddress != "192.168.5.42" {
		t.Fatalf("legacy migration lost metadata: %#v, %v", values, err)
	}
}
