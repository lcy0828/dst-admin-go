package modcontrol

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dont/internal/mods"
	"dont/shared"
)

type configFileRuntimeObserver struct {
	readyByTarget map[string]map[string]string // "targetId/installId" -> modId -> displayName
}

func (o *configFileRuntimeObserver) ObserveRuntimeModFiles(_ context.Context, req RuntimeFileRequest) (*shared.RuntimeModFilesObservation, error) {
	obs := &shared.RuntimeModFilesObservation{
		InstallationID: req.InstallationID,
		Mods:           make(map[string]shared.RuntimeModFileState),
	}
	ready := o.readyByTarget[req.TargetID+"/"+req.InstallationID]
	for _, id := range req.WorkshopIDs {
		if name, ok := ready[id]; ok {
			obs.Mods[id] = shared.RuntimeModFileState{
				Status: shared.RuntimeModFileReady,
				Name:   name,
			}
		} else {
			obs.Mods[id] = shared.RuntimeModFileState{
				Status: shared.RuntimeModFileMissing,
				Reason: "missing",
			}
		}
	}
	return obs, nil
}

func (o *configFileRuntimeObserver) ObserveRuntimeModInventory(_ context.Context, targetID, installationID string) (*shared.RuntimeModFilesObservation, error) {
	return o.ObserveRuntimeModFiles(context.Background(), RuntimeFileRequest{
		TargetID:       targetID,
		InstallationID: installationID,
	})
}

func TestConfigurationFilePreviewApplyAndDownloadAcrossLocalAndAgentWorlds(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &modConfigurationPublisherFixture{}
	if err := service.ConfigureConfigurationPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	fetcher := &parallelModFetcher{}
	if err := service.ConfigureInstallationContentFetcher(fetcher); err != nil {
		t.Fatal(err)
	}
	observer := &configFileRuntimeObserver{
		readyByTarget: map[string]map[string]string{
			"local/default": {
				"100": "Installed Master Mod",
			},
			"target-2/install-2": {
				"100": "Installed Master Mod",
			},
		},
	}
	if err := service.ConfigureRuntimeFileObserver(observer); err != nil {
		t.Fatal(err)
	}

	// 1. Read ConfigurationFile: includes WorldRevisions for both Master and Caves + TopologyRevision
	file, err := service.ConfigurationFile(context.Background(), fixture.room.ID, fixture.master.ID)
	if err != nil {
		t.Fatalf("ConfigurationFile failed: %v", err)
	}
	if len(file.WorldRevisions) != 2 || file.WorldRevisions[fixture.master.ID] == "" || file.WorldRevisions[fixture.caves.ID] == "" || file.TopologyRevision == "" {
		t.Fatalf("expected WorldRevisions and TopologyRevision, got %#v", file)
	}

	// 2. Reject accidental leveldataoverride.lua paste
	_, err = service.PreviewConfigurationFile(context.Background(), fixture.room.ID, fixture.master.ID, mods.ConfigurationFilePreviewRequest{
		Content:           `return { location = "forest", overrides = { day = "default" } }`,
		WorldIDs:          []string{fixture.master.ID, fixture.caves.ID},
		ExpectedRevisions: file.WorldRevisions,
	})
	var synErr *mods.FileSyntaxError
	if !errors.As(err, &synErr) || !strings.Contains(synErr.Message, "世界配置") {
		t.Fatalf("expected leveldataoverride paste guardrail error, got %v", err)
	}

	// 3. Client paste with KLEI header + comment + 1 ready mod (100) + 1 missing Workshop mod (378160973)
	pasted := "\xEF\xBB\xBFKLEI     1 return {\r\n" +
		"  -- keep my mod comment\r\n" +
		"  [\"workshop-100\"] = { enabled = true, configuration_options = {} },\r\n" +
		"  [\"workshop-378160973\"] = { enabled = true, configuration_options = { lang = \"zh\" } },\r\n" +
		"}\r\n"

	preview, err := service.PreviewConfigurationFile(context.Background(), fixture.room.ID, fixture.master.ID, mods.ConfigurationFilePreviewRequest{
		Content:                  pasted,
		WorldIDs:                 []string{fixture.master.ID, fixture.caves.ID},
		ExpectedRevisions:        file.WorldRevisions,
		ExpectedTopologyRevision: file.TopologyRevision,
	})
	if err != nil {
		t.Fatalf("PreviewConfigurationFile failed: %v", err)
	}
	if !reflect.DeepEqual(preview.MissingWorkshopIDs, []string{"378160973"}) {
		t.Fatalf("expected missingWorkshopIds=[378160973], got %v", preview.MissingWorkshopIDs)
	}
	if !strings.Contains(preview.NormalizedContent, "-- keep my mod comment") || strings.HasPrefix(preview.NormalizedContent, "KLEI") {
		t.Fatalf("expected stripped KLEI header and preserved comment, got:\n%s", preview.NormalizedContent)
	}

	// 4. Stale revision is rejected before writing any target
	staleRevs := map[string]string{
		fixture.master.ID: file.WorldRevisions[fixture.master.ID],
		fixture.caves.ID:  "stale-caves-revision",
	}
	_, err = service.ApplyConfigurationFile(context.Background(), fixture.room.ID, fixture.master.ID, mods.ConfigurationFileApplyRequest{
		Content:           pasted,
		WorldIDs:          []string{fixture.master.ID, fixture.caves.ID},
		ExpectedRevisions: staleRevs,
	})
	if !errors.Is(err, mods.ErrRevisionConflict) || len(publisher.writes) != 0 {
		t.Fatalf("expected ErrRevisionConflict with 0 writes, got err=%v writes=%#v", err, publisher.writes)
	}

	// 5. DownloadAndApplyConfigurationFile downloads 378160973 to both local and remote Agent, then publishes modoverrides.lua
	var out bytes.Buffer
	actionRes, err := service.DownloadAndApplyConfigurationFile(context.Background(), "job-mod-file-1", fixture.room.ID, fixture.master.ID, mods.ConfigurationFileApplyRequest{
		Content:                  pasted,
		WorldIDs:                 []string{fixture.master.ID, fixture.caves.ID},
		ExpectedRevisions:        file.WorldRevisions,
		ExpectedTopologyRevision: file.TopologyRevision,
		DownloadMissing:          true,
	}, &out)
	if err != nil {
		t.Fatalf("DownloadAndApplyConfigurationFile failed: %v", err)
	}
	if !reflect.DeepEqual(actionRes.ModIDs, []string{"378160973"}) {
		t.Fatalf("unexpected downloaded ModIDs: %#v", actionRes)
	}
	if !reflect.DeepEqual(fetcher.modIDsByInstallation["local/default"], []string{"378160973"}) ||
		!reflect.DeepEqual(fetcher.modIDsByInstallation["target-2/install-2"], []string{"378160973"}) {
		t.Fatalf("missing mod was not downloaded to both local and remote installations: %#v", fetcher.modIDsByInstallation)
	}
	masterWritten := string(publisher.writes[fixture.room.ID+"/"+fixture.master.ID])
	cavesWritten := string(publisher.writes[fixture.room.ID+"/"+fixture.caves.ID])
	if !strings.Contains(masterWritten, "-- keep my mod comment") || masterWritten != cavesWritten {
		t.Fatalf("published modoverrides mismatch:\nmaster=%s\ncaves=%s", masterWritten, cavesWritten)
	}
}
