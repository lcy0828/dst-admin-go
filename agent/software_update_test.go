package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dont/shared"
)

func TestAgentSoftwareRelayUsesAuthenticatedControllerAndBoundsDownload(t *testing.T) {
	payload := []byte("official Agent update archive")
	hash := sha256.Sum256(payload)
	for _, scenario := range []string{"success", "wrong_token", "wrong_size", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			id := strings.Repeat("a", 32)
			token := strings.Repeat("t", 32)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/agent-software-updates/"+id || r.Header.Get("Authorization") != "Bearer "+token {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if scenario == "redirect" {
					http.Redirect(w, r, "/unexpected", http.StatusFound)
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			request := shared.AgentUpgradeRequest{ReleaseID: id, Version: "v1.2.3", OS: "linux", Arch: "amd64", Source: "controller", DownloadPath: "/agent-software-updates/" + id, DownloadToken: token, SHA256: hex.EncodeToString(hash[:]), Size: int64(len(payload))}
			if scenario == "wrong_token" {
				request.DownloadToken = strings.Repeat("x", 32)
			}
			if scenario == "wrong_size" {
				request.Size++
			}
			client, err := newControllerUpdateClient(strings.Replace(server.URL, "http://", "ws://", 1)+"/agent?ignored=1", request)
			if err != nil {
				t.Fatal(err)
			}
			var received bytes.Buffer
			err = client.Download(context.Background(), client.release.Archive, "auto", &received)
			if scenario == "success" {
				if err != nil || !bytes.Equal(payload, received.Bytes()) || client.release.Archive.Digest != "sha256:"+request.SHA256 {
					t.Fatal("relay altered official archive or digest", err)
				}
			} else if err == nil {
				t.Fatal("invalid relay transfer accepted")
			}
			for _, path := range []string{"https://example.com/agent-software-updates/" + id, "//example.com/agent-software-updates/" + id, request.DownloadPath + "?override=1", "/agent-software-updates/../agent"} {
				invalid := request
				invalid.DownloadPath = path
				if _, err := newControllerUpdateClient("wss://controller.example/agent", invalid); err == nil {
					t.Fatal("untrusted relay path accepted", path)
				}
			}
		})
	}
}
