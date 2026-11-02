package mods

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestUnconfiguredInstalledModExposesDefaultsWithoutChangingWorld(t *testing.T) {
	for _, missingFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing-file", true: "missing-file"}[missingFile], func(t *testing.T) {
			service, backups, path := newConfigTestService(t)
			original := []byte("return { operator_value = { untouched = true } }\n")
			if err := os.WriteFile(path, original, 0640); err != nil {
				t.Fatal(err)
			}
			if missingFile {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			configuration, err := service.Configuration(context.Background(), "room-1", "world-1", "378160973")
			if err != nil || configuration.Configured || configuration.Enabled || len(configuration.Fields) != 2 ||
				configuration.Values["show_players"] != true || configuration.Values["position_color"] != "green" ||
				len(configuration.Overrides) != 0 || len(configuration.UnknownValues) != 0 || configuration.Revision == "" {
				t.Fatalf("default preview=%+v err=%v", configuration, err)
			}
			request := ConfigUpdateRequest{ExpectedRevision: configuration.Revision, Enabled: true,
				Patch: map[string]json.RawMessage{"show_players": json.RawMessage(`false`)}}
			if _, err := service.PreviewConfiguration(context.Background(), "room-1", "world-1", "378160973", request); !errors.Is(err, ErrModNotConfigured) {
				t.Fatalf("patch preview created an absent entry: %v", err)
			}
			if _, err := service.ApplyConfiguration(context.Background(), "test", "room-1", "world-1", "378160973", request); !errors.Is(err, ErrModNotConfigured) {
				t.Fatalf("apply created an absent entry: %v", err)
			}
			data, err := os.ReadFile(path)
			if missingFile {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("read created a configuration file: %v", err)
				}
			} else if err != nil || string(data) != string(original) {
				t.Fatalf("read changed existing configuration: %q, %v", data, err)
			}
			if backups.count != 0 {
				t.Fatal("reading defaults triggered a backup")
			}
		})
	}
}

func TestConfiguredDisabledModRemainsEditable(t *testing.T) {
	service, _, path := newConfigTestService(t)
	if err := os.WriteFile(path, []byte(`return { ["workshop-378160973"] = { enabled = false } }`), 0640); err != nil {
		t.Fatal(err)
	}
	configuration, err := service.Configuration(context.Background(), "room-1", "world-1", "378160973")
	if err != nil || !configuration.Configured || configuration.Enabled {
		t.Fatalf("disabled configuration=%+v err=%v", configuration, err)
	}
	_, err = service.ApplyConfiguration(context.Background(), "test", "room-1", "world-1", "378160973", ConfigUpdateRequest{
		ExpectedRevision: configuration.Revision, Enabled: false,
		Patch: map[string]json.RawMessage{"position_color": json.RawMessage(`"orange"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := service.Configuration(context.Background(), "room-1", "world-1", "378160973")
	if err != nil || !after.Configured || after.Enabled || after.Values["position_color"] != "orange" {
		t.Fatalf("saving enabled or lost disabled mod: %+v, %v", after, err)
	}
}
