package softwareupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/buildinfo"
)

func hashBytes(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }

func testBundle(t *testing.T, version, platform string, files map[string][]byte) []byte {
	t.Helper()
	if files == nil {
		files = map[string][]byte{"dst-admin": []byte("manager"), "dst-map-renderer": []byte("renderer"), "mod-local-setup": []byte("mod helper")}
	}
	manifest := Manifest{Protocol: Protocol, Version: version, Platform: platform, EmbeddedWebUI: true, FrontendCommit: strings.Repeat("a", 40), Files: map[string]FileDigest{}}
	for name, data := range files {
		manifest.Files[name] = FileDigest{Size: int64(len(data)), SHA256: hashBytes(data)}
	}
	data, _ := json.Marshal(manifest)
	all := map[string][]byte{"manifest.json": data}
	for name, data := range files {
		all[name] = data
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gz)
	for name, data := range all {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0700, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

type fixtureClient struct {
	release  *Release
	data     []byte
	checksum string
	failure  error
	block    <-chan struct{}
	calls    atomic.Int32
}

func (f *fixtureClient) Latest(context.Context, string, string) (*Release, error) {
	f.calls.Add(1)
	return f.release, f.failure
}
func (f *fixtureClient) Checksum(context.Context, Asset, string) ([]byte, error) {
	return []byte(f.checksum + "  " + f.release.Archive.Name + "\n"), nil
}
func (f *fixtureClient) Download(ctx context.Context, _ Asset, _ string, writer io.Writer) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err := writer.Write(f.data)
	return err
}

func testService(t *testing.T, data []byte) (*Service, *fixtureClient) {
	t.Helper()
	root := t.TempDir()
	if err := ensureRoot(root); err != nil {
		t.Fatal(err)
	}
	client := &fixtureClient{data: data, checksum: hashBytes(data), release: &Release{Version: "v1.1.0", OnlineUpdate: true, Archive: Asset{Name: "dst-admin-update-linux-amd64.tar.gz", Size: int64(len(data))}}}
	service := New(Config{Root: root, Current: buildinfo.Info{Version: "v1.0.0"}, BaseVersion: "v1.0.0", Platform: "linux-amd64", Managed: true, Ready: func() bool { return true }, Client: client, ValidateExecutable: func(context.Context, string, string, string) error { return nil }})
	if err := writeState(root, diskState{Protocol: Protocol, Current: &installedRelease{Version: "v1.0.0"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return service, client
}

func finishDownload(t *testing.T, service *Service) Snapshot {
	t.Helper()
	service.mu.Lock()
	done := service.done
	service.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("download timeout")
	}
	value, err := service.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPreparedUpdateSurvivesServiceRecreationAndPreservesUserData(t *testing.T) {
	service, _ := testService(t, testBundle(t, "v1.1.0", "linux-amd64", nil))
	sentinel := filepath.Join(service.config.Root, "save-sentinel")
	_ = os.WriteFile(sentinel, []byte("keep"), 0600)
	operation, err := service.Start("v1.1.0", "auto")
	if err != nil {
		t.Fatal(err)
	}
	status := finishDownload(t, service)
	if status.Operation.Phase != "prepared" || !releaseIDPattern.MatchString(status.Operation.ReleaseID) {
		t.Fatalf("not prepared: %+v", status.Operation)
	}
	// A new instance must use the persisted release ID, not in-memory state.
	service = New(service.config)
	var resumes int
	service.SetRestartGuard(func(context.Context) (func(), error) { return func() { resumes++ }, nil })
	if _, _, err := service.PrepareApply(context.Background(), "wrong"); !errors.Is(err, ErrNotPrepared) || resumes != 1 {
		t.Fatalf("invalid apply didn't resume: %v, %d", err, resumes)
	}
	value, _, err := service.PrepareApply(context.Background(), operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := readState(service.config.Root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending == nil || state.Pending.ID != status.Operation.ReleaseID || value.Phase != "restarting" {
		t.Fatalf("bad pending state: %+v", state)
	}
	if data, _ := os.ReadFile(sentinel); string(data) != "keep" {
		t.Fatal("user data changed")
	}
}

func TestUnverifiedPackagesNeverBecomePending(t *testing.T) {
	for _, kind := range []string{"checksum", "version", "platform", "executable", "missing-bundle"} {
		t.Run(kind, func(t *testing.T) {
			version, platform := "v1.1.0", "linux-amd64"
			if kind == "version" {
				version = "v1.2.0"
			}
			if kind == "platform" {
				platform = "darwin-arm64"
			}
			service, client := testService(t, testBundle(t, version, platform, nil))
			if kind == "checksum" {
				client.checksum = strings.Repeat("0", 64)
			}
			if kind == "executable" {
				service.config.ValidateExecutable = func(context.Context, string, string, string) error { return ErrInvalid }
			}
			if kind == "missing-bundle" {
				client.release.OnlineUpdate = false
			}
			if _, err := service.Start("v1.1.0", "direct"); err != nil {
				t.Fatal(err)
			}
			value := finishDownload(t, service)
			if value.Operation.Phase != "failed" || value.Operation.Error == "" {
				t.Fatalf("invalid package accepted: %+v", value.Operation)
			}
			state, _ := readState(service.config.Root)
			if state.Pending != nil || state.Current.Version != "v1.0.0" {
				t.Fatal("committed version changed")
			}
		})
	}
}

func TestBusyDownloadAndCancellation(t *testing.T) {
	service, client := testService(t, testBundle(t, "v1.1.0", "linux-amd64", nil))
	client.block = make(chan struct{})
	if _, err := service.Start("v1.1.0", "auto"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start("v1.1.0", "auto"); !errors.Is(err, ErrBusy) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, _, err := service.PrepareApply(context.Background(), "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unguarded apply: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	value := finishDownload(t, service)
	if value.Operation.Phase != "failed" {
		t.Fatal("cancel not persisted")
	}
}

func TestConfigurationPauseProtectsDownloadsAndRetainsPreparedPackage(t *testing.T) {
	service, client := testService(t, testBundle(t, "v1.1.0", "linux-amd64", nil))
	releaseDownload := make(chan struct{})
	client.block = releaseDownload
	if _, err := service.Start("v1.1.0", "auto"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PauseIfIdle(); !errors.Is(err, ErrBusy) {
		t.Fatalf("active download admitted a configuration reload: %v", err)
	}
	close(releaseDownload)
	prepared := finishDownload(t, service)
	resume, err := service.PauseIfIdle()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start("v1.1.0", "auto"); !errors.Is(err, ErrBusy) {
		t.Fatalf("download admitted during reload: %v", err)
	}
	resume()
	resume()
	retained, err := service.Start("v1.1.0", "auto")
	if err != nil || retained.ID != prepared.Operation.ID || retained.Phase != "prepared" {
		t.Fatalf("prepared package lost: %+v %v", retained, err)
	}
}

func TestPreparedOlderPackageCannotDowngradeNewerBase(t *testing.T) {
	service, _ := testService(t, testBundle(t, "v1.1.0", "linux-amd64", nil))
	if _, err := service.Start("v1.1.0", "auto"); err != nil {
		t.Fatal(err)
	}
	prepared := finishDownload(t, service)
	service.config.Current.Version = "v1.2.0"
	service.SetRestartGuard(func(context.Context) (func(), error) { return func() {}, nil })
	if _, _, err := service.PrepareApply(context.Background(), prepared.Operation.ID); !errors.Is(err, ErrNotPrepared) {
		t.Fatalf("older staged program accepted: %v", err)
	}
	state, _ := readState(service.config.Root)
	if state.Pending != nil {
		t.Fatal("downgrade published a pending switch")
	}
}

func TestVersionCheckCoalescesAndRetainsCachedReleaseOnFailure(t *testing.T) {
	service, client := testService(t, nil)
	for i := 0; i < 3; i++ {
		value, err := service.Check(context.Background(), false, "auto")
		if err != nil || !value.Check.HasUpdate {
			t.Fatal(value, err)
		}
	}
	if client.calls.Load() != 1 {
		t.Fatal("cached check requested network")
	}
	service.mu.Lock()
	service.check.CheckedAt = time.Now().Add(-time.Hour)
	service.mu.Unlock()
	client.failure = errors.New("offline")
	value, err := service.Check(context.Background(), true, "auto")
	if err != nil || value.Check.Latest.Version != "v1.1.0" || value.Check.Warning != "offline" || !value.Check.Cached {
		t.Fatal(value, err)
	}
}

func TestStrictVersions(t *testing.T) {
	for _, invalid := range []string{"v1.01.0", "v1.2", "v1.2.3-beta", "v4294967296.0.0", "../../1.2.3"} {
		if _, ok := NormalizeVersion(invalid); ok {
			t.Fatal(invalid)
		}
	}
	if CompareVersions("v1.10.0", "v1.9.9") != 1 || CompareVersions("2.0.0", "v10.0.0") != -1 || CompareVersions("v1.2.3", "1.2.3") != 0 {
		t.Fatal("version ordering")
	}
}

func TestStartupVerificationBlocksProgramSwitches(t *testing.T) {
	service, _ := testService(t, testBundle(t, "v1.1.0", "linux-amd64", nil))
	operation, err := service.Start("v1.1.0", "auto")
	if err != nil {
		t.Fatal(err)
	}
	finishDownload(t, service)
	service.config.Ready = func() bool { return false }
	service.SetRestartGuard(func(context.Context) (func(), error) {
		t.Fatal("entered restart guard during startup verification")
		return nil, nil
	})
	if _, _, err := service.PrepareApply(context.Background(), operation.ID); !errors.Is(err, ErrBusy) {
		t.Fatal("startup race admitted", err)
	}
	if _, err := service.Start("v1.1.0", "auto"); !errors.Is(err, ErrBusy) {
		t.Fatal("startup download admitted", err)
	}
	state, _ := readState(service.config.Root)
	if state.Pending != nil || state.Operation.Phase != "prepared" {
		t.Fatal("prepared package lost")
	}
}

func TestLowDiskSpacePreservesDataAndSkipsDownload(t *testing.T) {
	service, client := testService(t, testBundle(t, "v1.1.0", "linux-amd64", nil))
	service.config.FreeSpace = func(context.Context, string) (uint64, error) { return 1, nil }
	client.block = make(chan struct{}) // A download would never finish.
	if _, err := service.Start("v1.1.0", "auto"); err != nil {
		t.Fatal(err)
	}
	status := finishDownload(t, service)
	if status.Operation.Phase != "failed" || !strings.Contains(status.Operation.Error, "headroom") {
		t.Fatal(status.Operation)
	}
	state, _ := readState(service.config.Root)
	if state.Current.Version != "v1.0.0" || state.Pending != nil {
		t.Fatal("low space changed program")
	}
}
