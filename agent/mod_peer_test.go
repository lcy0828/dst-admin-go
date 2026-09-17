package agent

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"dont/internal/moddistribution"
	"dont/shared"
	"github.com/go-ini/ini"
)

func importPeerTestMod(t *testing.T, a *Agent, installation RuntimeInstallation, workshopID string, data []byte) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, workshopID)
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "modinfo.lua"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := a.modManager(installation)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := manager.Import(context.Background(), workshopID, source, moddistribution.Metadata{Title: "Peer Test"})
	if err != nil {
		t.Fatal(err)
	}
	return manifest.TreeSHA256
}

func TestModPeerGrantServesExactArtifactWithRange(t *testing.T) {
	a, installation := newModOperationAgent(t)
	a.Config.ModPeerListenAddr = "127.0.0.1:18081"
	server := httptest.NewServer(a.modPeerHandler())
	defer server.Close()
	a.Config.ModPeerAdvertiseURL = server.URL

	const workshopID = "1392778117"
	treeSHA := importPeerTestMod(t, a, installation, workshopID, []byte("name = 'peer artifact'\n"))
	location, err := a.issueModPeerGrant(context.Background(), installation, "agent:target", workshopID, treeSHA)
	if err != nil {
		t.Fatal(err)
	}
	if location.Source != shared.RuntimeModFetchSourcePeer || location.DownloadURL != server.URL+location.DownloadPath || location.Size < 2 {
		t.Fatalf("invalid peer location: %#v", location)
	}

	request, err := http.NewRequest(http.MethodGet, location.DownloadURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+location.DownloadToken)
	request.Header.Set("Range", "bytes=1-")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPartialContent || int64(len(body)) != location.Size-1 ||
		response.Header.Get("Content-Range") != fmt.Sprintf("bytes 1-%d/%d", location.Size-1, location.Size) ||
		response.Header.Get("X-DST-Mod-Tree-SHA256") != treeSHA {
		t.Fatalf("range status=%d bytes=%d headers=%v", response.StatusCode, len(body), response.Header)
	}
}

func TestModFetchFallsBackAcrossPeerLocations(t *testing.T) {
	a, _ := newModOperationAgent(t)
	data := []byte("name = 'peer fallback'\n")
	archive := testModTar(t, []testTarEntry{{name: "modinfo.lua", data: data, kind: tar.TypeReg}})
	treeSHA := testSingleFileTreeSHA("modinfo.lua", data)
	digest := sha256.Sum256(archive)
	bundleSHA := hex.EncodeToString(digest[:])
	var failedCalls, successfulCalls atomic.Int32
	failed := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		failedCalls.Add(1)
		http.Error(response, "unavailable", http.StatusServiceUnavailable)
	}))
	defer failed.Close()
	successful := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		successfulCalls.Add(1)
		if request.Header.Get("Authorization") != "Bearer peer-token-abcdefghijklmnopqrstuvwxyz" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
		_, _ = response.Write(archive)
	}))
	defer successful.Close()

	locations := []shared.RuntimeModFetchLocation{
		{
			Source: shared.RuntimeModFetchSourcePeer, DownloadURL: failed.URL + "/mod-peer/source-a/1392778117/" + treeSHA,
			DownloadPath: "/mod-peer/source-a/1392778117/" + treeSHA, DownloadToken: "peer-token-abcdefghijklmnopqrstuvwxyz",
			Size: int64(len(archive)), SHA256: bundleSHA,
		},
		{
			Source: shared.RuntimeModFetchSourcePeer, DownloadURL: successful.URL + "/mod-peer/source-b/1392778117/" + treeSHA,
			DownloadPath: "/mod-peer/source-b/1392778117/" + treeSHA, DownloadToken: "peer-token-abcdefghijklmnopqrstuvwxyz",
			Size: int64(len(archive)), SHA256: bundleSHA,
		},
	}
	sequence := 0
	result, err := executeModRequest(t, a, &sequence, shared.RuntimeActionModFetch, shared.RuntimeModRequest{
		WorkshopID: "1392778117", ExpectedTreeSHA256: treeSHA,
		FetchSources:   []shared.RuntimeModFetchSource{shared.RuntimeModFetchSourcePeer, shared.RuntimeModFetchSourcePeer},
		FetchLocations: locations,
	})
	if err != nil || !result.Complete || result.CacheManifest == nil || result.CacheManifest.TreeSHA256 != treeSHA ||
		result.FetchSource != shared.RuntimeModFetchSourcePeer || failedCalls.Load() != 1 || successfulCalls.Load() != 1 {
		t.Fatalf("result=%#v failed=%d successful=%d err=%v", result, failedCalls.Load(), successfulCalls.Load(), err)
	}
}

func TestModPeerConfigurationRequiresListenAndAdvertiseTogether(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.conf")
	if err := os.WriteFile(configPath, []byte("[agent]\nSERVER_URL = ws://127.0.0.1:8081/agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewAgent(&Config{
		AgentID: "peer-config-test", KeyFile: configPath, ModPeerListenAddr: "127.0.0.1:18081",
	})
	if err == nil || !strings.Contains(err.Error(), "必须同时配置") {
		t.Fatalf("partial peer configuration error=%v", err)
	}
}

func TestSaveConfigPersistsModPeerConfiguration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.conf")
	a := &Agent{Config: &Config{
		KeyFile: configPath, ModPeerListenAddr: ":18081", ModPeerAdvertiseURL: "http://192.0.2.10:18081/",
	}}
	if err := a.saveConfig(configPath, "ws://controller.example.test/agent", ""); err != nil {
		t.Fatalf("save config: %v", err)
	}
	config, err := ini.Load(configPath)
	if err != nil {
		t.Fatalf("load saved config: %v", err)
	}
	section := config.Section("agent")
	if got := section.Key("MOD_PEER_LISTEN_ADDR").String(); got != ":18081" {
		t.Fatalf("listen address=%q", got)
	}
	if got := section.Key("MOD_PEER_ADVERTISE_URL").String(); got != "http://192.0.2.10:18081" {
		t.Fatalf("advertise URL=%q", got)
	}
}
