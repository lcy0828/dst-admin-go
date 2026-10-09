package softwareupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func agentFixture(t *testing.T, directory, platform, version string, executable []byte) string {
	t.Helper()
	manifest := Manifest{Kind: "agent", Protocol: Protocol, Version: version, Platform: platform, Files: map[string]FileDigest{}}
	files := map[string][]byte{}
	for name := range filesFor("agent", platform) {
		data := []byte("packaged helper")
		if name == binaryFor("agent", platform) {
			data = executable
		}
		hash := sha256.Sum256(data)
		manifest.Files[name] = FileDigest{Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
		files[name] = data
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	files["manifest.json"] = data
	archive := filepath.Join(directory, "agent-fixture.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	for name, data := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0700, Typeflag: tar.TypeReg, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestAgentDownloadsVerifyRoleAndChecksumBeforePreparing(t *testing.T) {
	for _, scenario := range []string{"direct", "controller", "bad_checksum", "manager_package"} {
		t.Run(scenario, func(t *testing.T) {
			archive := agentFixture(t, t.TempDir(), "linux-amd64", "v1.1.0", []byte("new Agent"))
			data, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "manager_package" {
				data = testBundle(t, "v1.1.0", "linux-amd64", nil)
			}
			service, client := testService(t, data)
			service.config.Kind = "agent"
			client.release.Archive.Name = "dst-admin-agent-update-linux-amd64.tar.gz"
			if scenario == "bad_checksum" {
				client.checksum = strings.Repeat("0", 64)
			}
			save := filepath.Join(service.config.Root, "save-sentinel")
			if err := os.WriteFile(save, []byte("keep save"), 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "controller" {
				// A one-time relay must not change subsequent Agent release checks.
				service.config.Client = &fixtureClient{failure: ErrInvalid}
				_, err = service.StartWithClient("v1.1.0", "auto", client)
			} else {
				_, err = service.Start("v1.1.0", "direct")
			}
			if err != nil {
				t.Fatal(err)
			}
			status := finishDownload(t, service)
			expected := "prepared"
			if scenario == "bad_checksum" || scenario == "manager_package" {
				expected = "failed"
			}
			if status.Operation == nil || status.Operation.Phase != expected {
				t.Fatalf("download result: %+v", status.Operation)
			}
			state, err := readState(service.config.Root)
			if err != nil || state.Pending != nil || state.Current.Version != "v1.0.0" {
				t.Fatalf("download changed active program: %+v, %v", state, err)
			}
			if data, err := os.ReadFile(save); err != nil || string(data) != "keep save" {
				t.Fatal("download changed save data", err)
			}
			if scenario == "controller" {
				status, err := service.Check(context.Background(), true, "direct")
				if err != nil || status.Check.Warning == "" {
					t.Fatal("relay replaced default release client", err)
				}
			}
		})
	}
}

func TestAgentBundlesEnforceRolePlatformAndIntegrity(t *testing.T) {
	for _, platform := range []string{"linux-amd64", "linux-arm64", "darwin-arm64", "darwin-amd64", "windows-amd64"} {
		t.Run(platform, func(t *testing.T) {
			root := t.TempDir()
			archive := agentFixture(t, root, platform, "v1.2.3", []byte("Agent program"))
			stage := filepath.Join(root, "stage")
			if err := os.Mkdir(stage, 0700); err != nil {
				t.Fatal(err)
			}
			if err := extractBundleFor(archive, stage, "agent", platform); err != nil {
				t.Fatal(err)
			}
			if _, err := validateBundleFor(stage, "v1.2.3", platform, "agent"); err != nil {
				t.Fatal(err)
			}
			if _, err := validateBundle(stage, "v1.2.3", platform); err == nil {
				t.Fatal("Agent package accepted as management program")
			}
			if _, err := validateBundleFor(stage, "v1.2.3", "wrong-platform", "agent"); err == nil {
				t.Fatal("wrong platform accepted")
			}
			if err := os.WriteFile(filepath.Join(stage, binaryFor("agent", platform)), []byte("tampered bytes"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := validateBundleFor(stage, "v1.2.3", platform, "agent"); err == nil {
				t.Fatal("tampered Agent accepted")
			}
		})
	}
}
