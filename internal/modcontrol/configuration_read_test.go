package modcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dont/internal/mods"
	"dont/internal/runtimedriver"
	"dont/shared"
)

func TestRemoteSchemaIsUsedForReadPreviewAndSaveWithoutLocalFallback(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.driver.schemaHook = func(_ context.Context, target runtimedriver.Target, modID string) (shared.RuntimeModSchema, error) {
		if target.TargetID != "target-2" || target.InstallationID != "install-2" || target.WorldID != fixture.caves.ID {
			t.Fatalf("wrong schema target: %#v", target)
		}
		return shared.RuntimeModSchema{WorkshopID: modID, Parser: "go", Options: []interface{}{map[string]interface{}{
			"name": "remote_only", "default": false,
		}}}, nil
	}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &modConfigurationPublisherFixture{}
	if err := service.ConfigureConfigurationPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	configuration, err := service.Configuration(context.Background(), fixture.room.ID, fixture.caves.ID, "200")
	if err != nil || len(configuration.Fields) != 1 || configuration.Fields[0].Key != "remote_only" {
		t.Fatalf("configuration=%#v, err=%v", configuration, err)
	}
	request := mods.ConfigUpdateRequest{ExpectedRevision: configuration.Revision, PreserveEnabled: true,
		Patch: map[string]json.RawMessage{"remote_only": json.RawMessage(`true`)}}
	preview, err := service.PreviewConfiguration(context.Background(), fixture.room.ID, fixture.caves.ID, "200", request)
	if err != nil || len(preview.Changes) != 1 {
		t.Fatalf("preview=%#v, err=%v", preview, err)
	}
	if _, err := service.ApplyConfiguration(context.Background(), fixture.room.ID, fixture.caves.ID, "200", request); err != nil {
		t.Fatal(err)
	}
	if len(publisher.writes) != 1 || len(fixture.driver.schemaCalls) != 3 || len(fixture.mods.configurationContent) != 0 {
		t.Fatalf("writes=%#v, schema calls=%#v, local content=%s", publisher.writes, fixture.driver.schemaCalls, fixture.mods.configurationContent)
	}
	if fixture.driver.fetchCalls != 0 || fixture.driver.downloadCalls != 0 {
		t.Fatal("configuration caused a download")
	}
	fixture.driver.schemaHook = func(context.Context, runtimedriver.Target, string) (shared.RuntimeModSchema, error) {
		return shared.RuntimeModSchema{}, mods.ErrModInfoUnavailable
	}
	if _, err := service.Configuration(context.Background(), fixture.room.ID, fixture.caves.ID, "200"); !errors.Is(err, mods.ErrModInfoUnavailable) || len(fixture.mods.configurationContent) != 0 {
		t.Fatalf("remote failure fell back locally: %v", err)
	}
}

func TestLocalConfigurationDoesNotReadAnotherWorldOnRemoteNode(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.driver.overrideHook = func(runtimedriver.Target, string, string, int64) (shared.RuntimeModOverridesChunk, error) {
		return shared.RuntimeModOverridesChunk{}, errors.New("remote node is offline")
	}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Configuration(context.Background(), fixture.room.ID, fixture.master.ID, "100"); err != nil {
		t.Fatal(err)
	}
	if len(fixture.driver.overrideCalls) != 0 || len(fixture.driver.schemaCalls) != 0 {
		t.Fatal("local configuration contacted an unrelated remote world")
	}
}
