package modcontrol

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"dont/internal/mods"
	"dont/shared"
)

type publicItemCatalog struct {
	installationCatalogFixture
	provider      *mods.SteamProvider
	explicitCalls int
}

func (c *publicItemCatalog) DescribeForDownload(ctx context.Context, ids []string) (map[string]mods.SteamMod, error) {
	c.explicitCalls++
	return c.provider.BasicDetails(ctx, ids)
}

func (c *publicItemCatalog) UpdateLibrary(_ context.Context, id string, _ io.Writer) (mods.ActionResult, error) {
	c.updateCalls++
	return mods.ActionResult{ModIDs: []string{id}}, nil
}

func TestFetchLatestArtifactsUsesExplicitPublicMetadataOnLocalAndAgent(t *testing.T) {
	for _, target := range []string{"local", "agent:test-node"} {
		for _, available := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/available=%t", target, available), func(t *testing.T) {
				fixture := newRuntimeFixture(t, 64)
				id := fixture.artifact.WorkshopID
				pages := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/sharedfiles/filedetails/" {
						pages++
						fmt.Fprintf(w, `<html><body><div class="workshopItemTitle">Public Beta</div><div id="highlightContent">Description</div><a id="SubscribeItemBtn" onclick="SubscribeItem('%s','322330');">Subscribe</a></body></html>`, id)
						return
					}
					result := 15
					if available {
						result = 9
					}
					fmt.Fprintf(w, `{"response":{"publishedfiledetails":[{"publishedfileid":%q,"result":%d}]}}`, id, result)
				}))
				defer server.Close()
				provider := mods.NewSteamProvider("", "322330")
				provider.APIBase, provider.CommunityBase = server.URL, server.URL
				manifest := shared.RuntimeModCacheManifest{
					WorkshopID: id, TreeSHA256: fixture.artifact.TreeSHA256,
					ManifestSHA256: fixture.artifact.ManifestSHA256, Size: fixture.artifact.Size, FileCount: fixture.artifact.FileCount,
				}
				driver := &testModDriver{fetchManifest: &manifest, inspect: map[string]shared.RuntimeModCacheManifest{id + "\x00" + fixture.artifact.TreeSHA256: manifest}}
				runtime, err := NewRuntime(&SnapshotSource{}, fixture.manager, driver, fixture.cache, fixture.transfers)
				if err != nil {
					t.Fatal(err)
				}
				catalog := &publicItemCatalog{provider: provider}
				content, err := NewContentSource(fixture.manager, filepath.Dir(fixture.workshop))
				if err != nil {
					t.Fatal(err)
				}
				updater, err := NewInstallationUpdater(catalog, content, runtime, &installationLeaseFixture{})
				if err != nil {
					t.Fatal(err)
				}
				artifacts, err := updater.FetchLatestArtifacts(context.Background(), target, "primary", []string{id}, io.Discard)
				if catalog.explicitCalls != 1 || catalog.describeCalls != 0 {
					t.Fatalf("explicit metadata was bypassed: %+v", catalog)
				}
				if !available {
					if err == nil || len(artifacts) != 0 || pages != 0 || catalog.updateCalls != 0 || driver.fetchCalls != 0 {
						t.Fatalf("denied item downloaded: err=%v artifacts=%+v pages=%d update=%d fetch=%d", err, artifacts, pages, catalog.updateCalls, driver.fetchCalls)
					}
					return
				}
				if err != nil || len(artifacts) != 1 || artifacts[0].TreeSHA256 != fixture.artifact.TreeSHA256 || pages != 1 {
					t.Fatalf("public item not prepared and verified: artifacts=%+v err=%v pages=%d", artifacts, err, pages)
				}
				if target == "local" {
					if catalog.updateCalls != 1 || driver.fetchCalls != 0 {
						t.Fatalf("local download routed incorrectly: update=%d remote=%d", catalog.updateCalls, driver.fetchCalls)
					}
				} else if catalog.updateCalls != 0 || driver.fetchCalls != 1 || driver.fetchTarget.TargetID != target || driver.fetchMetadata.Title != "Public Beta" {
					t.Fatalf("Agent download routed incorrectly: update=%d fetch=%d metadata=%+v", catalog.updateCalls, driver.fetchCalls, driver.fetchMetadata)
				}
				if driver.fetchMetadata.Version != "" || driver.fetchMetadata.SteamManifestID != "" || !driver.fetchMetadata.SteamUpdatedAt.IsZero() {
					t.Fatalf("HTML invented Agent version evidence: %+v", driver.fetchMetadata)
				}
			})
		}
	}
}
