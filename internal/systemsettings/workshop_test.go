package systemsettings

import (
	"context"
	"errors"
	"testing"

	"dont/internal/mods"
)

func TestWorkshopLiveSettingsBypassWorldStopBarrierAndMaskProxy(t *testing.T) {
	s, err := NewService(NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	s.lookupEnv = func(string) (string, bool) { return "", false }
	barrierCalls, commits, discards := 0, 0, 0
	s.SetRuntimeApplier(func(context.Context, func() error, func() error) error { barrierCalls++; return ErrRuntimeBusy })
	var effective mods.NetworkConfig
	s.SetWorkshopApplier(func(config mods.NetworkConfig) (func(), func(), error) {
		return func() { commits++; effective = config }, func() { discards++ }, nil
	})
	before, _ := s.Settings()
	result, err := s.ApplyWorkshop(context.Background(), Input{Revision: before.Revision, Values: map[string]string{"mod.proxyMode": "custom", "mod.proxyURL": "http://user:password@127.0.0.1:7890", "mod.steamAPIKey": "replacement-key", "mod.steamAPIBase": "https://mirror.example.test/api", "mod.steamCommunityBase": "https://mirror.example.test/community"}})
	if err != nil || barrierCalls != 0 || commits != 1 || discards != 1 || effective.APIKey != "replacement-key" || effective.ProxyURL == "" || effective.APIBase != "https://mirror.example.test/api" || effective.CommunityBase != "https://mirror.example.test/community" {
		t.Fatalf("err=%v barrier=%d commits=%d config=%+v", err, barrierCalls, commits, effective)
	}
	for _, field := range result.Settings.Fields {
		if field.ID == "mod.proxyURL" && (field.Value != "" || !field.Configured || field.RestartRequired) {
			t.Fatalf("exposed proxy: %+v", field)
		}
	}
	// The original settings endpoint also updates the same live client.
	_, err = s.Apply(Input{Revision: result.Settings.Revision, Confirmation: ApplyConfirmation, Values: map[string]string{"mod.steamAPIKey": "another-valid-key"}})
	if err != nil || effective.APIKey != "another-valid-key" || barrierCalls != 0 {
		t.Fatalf("legacy key editor: %v", err)
	}
}

func TestWorkshopTestDoesNotWriteAndKeepsOrClearsSecretsExplicitly(t *testing.T) {
	s, _ := NewService(NewMemoryRepository())
	s.lookupEnv = func(string) (string, bool) { return "", false }
	before, _ := s.Settings()
	config, err := s.ResolveWorkshopTest(Input{Revision: before.Revision, Values: map[string]string{"mod.steamAPIBase": "http://proxy.example.test/api", "mod.steamAPIKey": ""}})
	if err != nil || config.APIKey != "test-steam-api-key" || config.APIBase != "http://proxy.example.test/api" {
		t.Fatalf("resolve=%+v err=%v", config, err)
	}
	config, err = s.ResolveWorkshopTest(Input{Revision: before.Revision, Values: map[string]string{}, ClearSecrets: []string{"mod.steamAPIKey"}})
	if err != nil || config.APIKey != "" {
		t.Fatalf("clear=%+v err=%v", config, err)
	}
	after, _ := s.Settings()
	if before.Revision != after.Revision {
		t.Fatal("test modified persisted configuration")
	}
	_, err = s.ResolveWorkshopTest(Input{Revision: "stale", Values: map[string]string{}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale test=%v", err)
	}
	_, err = s.ApplyWorkshop(context.Background(), Input{Revision: before.Revision, Values: map[string]string{"paths.save": "/different"}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("changed unrelated settings: %v", err)
	}
}

func TestWorkshopInvalidURLsAndEnvironmentOverrideCannotBeSaved(t *testing.T) {
	s, _ := NewService(NewMemoryRepository())
	s.lookupEnv = func(name string) (string, bool) {
		if name == "DST_ADMIN_STEAM_API_BASE" {
			return "https://managed.example.test", true
		}
		return "", false
	}
	before, _ := s.Settings()
	_, err := s.ApplyWorkshop(context.Background(), Input{Revision: before.Revision, Values: map[string]string{"mod.steamAPIBase": "https://other.example.test"}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("environment overwritten: %v", err)
	}
	s.lookupEnv = func(string) (string, bool) { return "", false }
	for _, raw := range []string{"file:///etc/passwd", "https://user:secret@api.example.test", "https://api.example.test?key=secret", "https://api.example.test/#fragment"} {
		_, err = s.ApplyWorkshop(context.Background(), Input{Revision: before.Revision, Values: map[string]string{"mod.steamAPIBase": raw}})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("accepted %q: %v", raw, err)
		}
	}
	after, _ := s.Settings()
	if before.Revision != after.Revision {
		t.Fatal("invalid configuration was persisted")
	}
}
