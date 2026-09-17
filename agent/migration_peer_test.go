package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dont/shared"
)

func TestMigrationPeerGrantBindsTargetAndResumesRangeImport(t *testing.T) {
	source, sourceInstallation := newModOperationAgent(t)
	target, targetInstallation := newModOperationAgent(t)
	source.Config.AgentID = "source-node"
	target.Config.AgentID = "target-node"
	source.Config.ModPeerListenAddr = "127.0.0.1:18081"
	cluster := filepath.Join(sourceInstallation.SavePath, "Cluster_1")
	for path, data := range map[string]string{
		filepath.Join(cluster, "cluster.ini"):                            "[NETWORK]\ncluster_name=Peer\n",
		filepath.Join(cluster, "Master", "server.ini"):                   "[SHARD]\nis_master=true\n",
		filepath.Join(cluster, "Master", "save", "session", "0001", "1"): strings.Repeat("save-data", 4096),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	migrationID := "migration-peer-agent-0001"
	sourceManager, err := source.transferManager(sourceInstallation)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := sourceManager.PrepareExport(context.Background(), migrationID, "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	var rangeHeader atomic.Value
	handler := source.modPeerHandler()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/migration-peer/") {
			rangeHeader.Store(request.Header.Get("Range"))
		}
		handler.ServeHTTP(response, request)
	}))
	defer server.Close()
	source.Config.ModPeerAdvertiseURL = server.URL
	grantRequest := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "migration-peer-grant-operation",
		InstallationID: sourceInstallation.ID, Action: shared.RuntimeActionMigrationPeerGrant,
		Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		Migration: &shared.RuntimeMigrationRequest{
			MigrationID: migrationID, PeerSubject: "agent:target-node", Size: descriptor.Size, SHA256: descriptor.SHA256,
		},
	}
	grantResult, err := source.executeRuntimeOperation(string(grantRequest.Action), &grantRequest, 60)
	if err != nil || grantResult.Migration == nil || grantResult.Migration.FetchLocation == nil {
		t.Fatalf("grant result=%#v err=%v", grantResult, err)
	}
	location := *grantResult.Migration.FetchLocation

	unauthorized, err := http.NewRequest(http.MethodHead, location.DownloadURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Header.Set("Authorization", "Bearer "+location.DownloadToken)
	unauthorized.Header.Set("X-DST-Peer-Subject", "agent:other-node")
	response, err := http.DefaultClient.Do(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong target status=%d", response.StatusCode)
	}

	targetManager, err := target.transferManager(targetInstallation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetManager.BeginImport(migrationID, descriptor.Size, descriptor.SHA256); err != nil {
		t.Fatal(err)
	}
	_, export, err := sourceManager.OpenExport(migrationID)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(export)
	_ = export.Close()
	if err != nil {
		t.Fatal(err)
	}
	half := len(archive) / 2
	if next, err := targetManager.ReceiveImport(context.Background(), migrationID, 0, bytes.NewReader(archive[:half])); next != int64(half) || err == nil {
		t.Fatalf("partial next=%d err=%v", next, err)
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	fetchRequest := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "migration-peer-fetch-operation", OperationKey: "migration-peer-fetch-key",
		InstallationID: targetInstallation.ID, Action: shared.RuntimeActionMigrationFetch,
		Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		LeaseID: "migration-peer-lease", FencingToken: 1, LeaseExpiresAt: &expires,
		Migration: &shared.RuntimeMigrationRequest{
			MigrationID: migrationID, Size: descriptor.Size, SHA256: descriptor.SHA256,
			FetchLocations: []shared.RuntimeMigrationFetchLocation{location},
		},
	}
	fetchResult, err := target.executeRuntimeOperation(string(fetchRequest.Action), &fetchRequest, 1800)
	if err != nil || fetchResult.Migration == nil || fetchResult.Migration.NextOffset != descriptor.Size {
		t.Fatalf("fetch result=%#v size=%d err=%v", fetchResult, descriptor.Size, err)
	}
	if got, _ := rangeHeader.Load().(string); got != fmt.Sprintf("bytes=%d-", half) {
		t.Fatalf("Range=%q", got)
	}
	if _, err := targetManager.VerifyImport(context.Background(), migrationID); err != nil {
		t.Fatal(err)
	}
	releaseRequest := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "migration-peer-release-operation", OperationKey: "migration-peer-release-key",
		InstallationID: sourceInstallation.ID, Action: shared.RuntimeActionMigrationExportRelease,
		Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		LeaseID: "migration-peer-source-lease", FencingToken: 1, LeaseExpiresAt: &expires,
		Migration: &shared.RuntimeMigrationRequest{MigrationID: migrationID},
	}
	if _, err := source.executeRuntimeOperation(string(releaseRequest.Action), &releaseRequest, 60); err != nil {
		t.Fatal(err)
	}
	revoked, err := http.NewRequest(http.MethodHead, location.DownloadURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	revoked.Header.Set("Authorization", "Bearer "+location.DownloadToken)
	revoked.Header.Set("X-DST-Peer-Subject", "agent:target-node")
	response, err = http.DefaultClient.Do(revoked)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked grant status=%d", response.StatusCode)
	}
}

func TestMigrationPeerGrantInitializesGrantStoreDefensively(t *testing.T) {
	source, installation := newModOperationAgent(t)
	source.Config.ModPeerListenAddr = "127.0.0.1:18081"
	source.Config.ModPeerAdvertiseURL = "http://127.0.0.1:18081"
	source.migrationPeerGrants = nil
	cluster := filepath.Join(installation.SavePath, "Cluster_1")
	for path, data := range map[string]string{
		filepath.Join(cluster, "cluster.ini"):          "[NETWORK]\ncluster_name=Peer\n",
		filepath.Join(cluster, "Master", "server.ini"): "[SHARD]\nis_master=true\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := source.transferManager(installation)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := manager.PrepareExport(context.Background(), "migration-peer-agent-0002", "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	location, err := source.issueMigrationPeerGrant(installation, shared.RuntimeMigrationRequest{
		MigrationID: descriptor.MigrationID, PeerSubject: "agent:target-node", Size: descriptor.Size, SHA256: descriptor.SHA256,
	})
	if err != nil || location.DownloadToken == "" || len(source.migrationPeerGrants) != 1 {
		t.Fatalf("location=%#v grants=%d err=%v", location, len(source.migrationPeerGrants), err)
	}
}
