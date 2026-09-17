package modcontrol

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/shared"
)

type installationCatalogFixture struct {
	describeCalls int
	updateCalls   int
	downloadCalls int
}

func (f *installationCatalogFixture) Describe(_ context.Context, modIDs []string) (map[string]mods.SteamMod, error) {
	f.describeCalls++
	result := make(map[string]mods.SteamMod, len(modIDs))
	for _, modID := range modIDs {
		result[modID] = mods.SteamMod{ID: modID, Name: "Remote Mod"}
	}
	return result, nil
}

func (f *installationCatalogFixture) UpdateLibrary(context.Context, string, io.Writer) (mods.ActionResult, error) {
	f.updateCalls++
	return mods.ActionResult{}, errors.New("Controller download must not run for a remote installation")
}

func (f *installationCatalogFixture) DownloadLibraryMods(_ context.Context, modIDs []string, _ io.Writer) (mods.ActionResult, error) {
	f.downloadCalls++
	return mods.ActionResult{ModIDs: append([]string(nil), modIDs...)}, nil
}

type installationContentFixture struct{ resolveCalls int }

func (f *installationContentFixture) Resolve(context.Context, modpublication.ModRequirement) (modpublication.ContentArtifact, error) {
	f.resolveCalls++
	return modpublication.ContentArtifact{}, errors.New("Controller content must not be resolved for a remote installation")
}

type installationLeaseFixture struct{ next uint64 }

func (f *installationLeaseFixture) Acquire(_ context.Context, resourceID, operationKey string, ttl time.Duration) (modpublication.Fence, error) {
	f.next++
	return modpublication.Fence{
		RoomID: resourceID, LeaseID: "installation-lease", OperationKey: operationKey,
		FencingToken: f.next, ExpiresAt: time.Now().UTC().Add(ttl),
	}, nil
}

func (f *installationLeaseFixture) Renew(_ context.Context, fence modpublication.Fence, ttl time.Duration) (modpublication.Fence, error) {
	fence.ExpiresAt = time.Now().UTC().Add(ttl)
	return fence, nil
}

func (*installationLeaseFixture) Release(modpublication.Fence) error { return nil }

func TestFetchLatestArtifactsDownloadsOnRemoteInstallationWithoutControllerContent(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	manifest := shared.RuntimeModCacheManifest{
		WorkshopID: fixture.artifact.WorkshopID, TreeSHA256: fixture.artifact.TreeSHA256,
		ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
	}
	driver := &testModDriver{
		fetchManifest: &manifest,
		inspect: map[string]shared.RuntimeModCacheManifest{
			fixture.artifact.WorkshopID + "\x00" + fixture.artifact.TreeSHA256: manifest,
		},
	}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &installationCatalogFixture{}
	content := &installationContentFixture{}
	updater, err := NewInstallationUpdater(catalog, content, runtime, &installationLeaseFixture{})
	if err != nil {
		t.Fatal(err)
	}

	artifacts, err := updater.FetchLatestArtifacts(context.Background(), "agent:debian12", "native", []string{fixture.artifact.WorkshopID}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].TreeSHA256 != fixture.artifact.TreeSHA256 {
		t.Fatalf("artifacts=%#v", artifacts)
	}
	if catalog.describeCalls != 1 || catalog.updateCalls != 0 || content.resolveCalls != 0 {
		t.Fatalf("Controller calls: describe=%d update=%d resolve=%d", catalog.describeCalls, catalog.updateCalls, content.resolveCalls)
	}
	if driver.fetchCalls != 1 || driver.fetchTarget.TargetID != "agent:debian12" || driver.fetchTarget.InstallationID != "native" ||
		driver.fetchTreeSHA != "" || len(driver.fetchLocations) != 1 || len(driver.fetchLocations[0]) != 0 {
		t.Fatalf("remote fetch: calls=%d target=%#v tree=%q locations=%#v", driver.fetchCalls, driver.fetchTarget, driver.fetchTreeSHA, driver.fetchLocations)
	}
}

func TestUpdateInstallationModsUsesOneDirectRemoteDownload(t *testing.T) {
	fixture := newRuntimeFixture(t, 64)
	driver := &testModDriver{}
	runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &installationCatalogFixture{}
	content := &installationContentFixture{}
	updater, err := NewInstallationUpdater(catalog, content, runtime, &installationLeaseFixture{})
	if err != nil {
		t.Fatal(err)
	}

	result, err := updater.UpdateInstallationMods(context.Background(), "agent:debian12", "native", []string{"222", "111", "222"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ModIDs) != 2 || result.ModIDs[0] != "111" || result.ModIDs[1] != "222" {
		t.Fatalf("result=%#v", result)
	}
	if driver.downloadCalls != 1 || driver.downloadTarget.TargetID != "agent:debian12" || driver.downloadTarget.InstallationID != "native" ||
		len(driver.downloadIDs) != 2 || driver.downloadIDs[0] != "111" || driver.downloadIDs[1] != "222" {
		t.Fatalf("remote download calls=%d target=%#v ids=%#v", driver.downloadCalls, driver.downloadTarget, driver.downloadIDs)
	}
	if catalog.describeCalls != 0 || catalog.updateCalls != 0 || catalog.downloadCalls != 0 || content.resolveCalls != 0 {
		t.Fatalf("direct remote download used Controller content: catalog=%#v content=%#v", catalog, content)
	}
	if err := updater.LinkInstallationMods(context.Background(), "agent:debian12", "native", []string{"111"}); err != nil {
		t.Fatal(err)
	}
	if driver.downloadCalls != 1 || driver.linkCalls != 1 || catalog.describeCalls != 0 {
		t.Fatalf("cached link used download or metadata: download=%d link=%d describe=%d", driver.downloadCalls, driver.linkCalls, catalog.describeCalls)
	}
}

func TestInstallationCatalogProgressKeepsNestedCompletionInInspectStage(t *testing.T) {
	updates := make([]operationprogress.Update, 0, 3)
	ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) {
		updates = append(updates, update)
	})
	itemCtx := withInstallationCatalogProgress(ctx, 1, 2)
	operationprogress.Report(itemCtx, operationprogress.Update{Stage: operationprogress.StageModInspect, Percent: 100, Message: "inspect"})
	operationprogress.Report(itemCtx, operationprogress.Update{Stage: operationprogress.StageModCache, Percent: 85, Message: "download"})
	operationprogress.Report(itemCtx, operationprogress.Update{Stage: operationprogress.StageModDone, Percent: 100, Message: "done"})

	want := []int{55, 91, 100}
	if len(updates) != len(want) {
		t.Fatalf("updates=%#v", updates)
	}
	for index, update := range updates {
		if update.Stage != operationprogress.StageModInspect || update.Percent != want[index] {
			t.Fatalf("update[%d]=%#v want percent=%d", index, update, want[index])
		}
	}
}
