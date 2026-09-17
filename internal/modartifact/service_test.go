package modartifact

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/moddistribution"

	"github.com/shirou/gopsutil/v3/disk"
)

func newArtifactFixture(t *testing.T) (*Service, moddistribution.Manifest) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache, state := filepath.Join(root, "cache"), filepath.Join(root, "state")
	server, saves := filepath.Join(root, "server"), filepath.Join(root, "saves")
	for _, directory := range []string{cache, state, server, saves} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := moddistribution.New(moddistribution.Config{
		CacheRoot: cache, StateRoot: state, NodeID: "controller", ReserveBytes: 0,
		Installations: []moddistribution.TrustedInstallation{{
			ID: "default", NodeID: "controller", ServerPath: server, SavePath: saves,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "modinfo.lua"), []byte("name='artifact test'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "scripts", "main.lua"), []byte("return true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := manager.Import(context.Background(), "1392778117", source, moddistribution.Metadata{Title: "Artifact Test"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(manager, filepath.Join(root, "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, _ error) error {
			if entry != nil {
				if entry.IsDir() {
					_ = os.Chmod(path, 0o700)
				} else {
					_ = os.Chmod(path, 0o600)
				}
			}
			return nil
		})
	})
	return service, manifest
}

func TestArtifactBundleIsPersistentAndGrantIsScoped(t *testing.T) {
	service, manifest := newArtifactFixture(t)
	clock := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return clock }
	location, token, err := service.Issue(context.Background(), "agent:node-one", manifest.WorkshopID, manifest.TreeSHA256, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if location.Size < 1 || location.SHA256 == "" || location.DownloadToken != token {
		t.Fatalf("invalid artifact location: %#v", location)
	}
	descriptor, file, err := service.Open(manifest.WorkshopID, manifest.TreeSHA256, token)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(file)
	files := 0
	for {
		header, readErr := reader.Next()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			file.Close()
			t.Fatal(readErr)
		}
		if header.Typeflag == tar.TypeReg {
			files++
		}
	}
	file.Close()
	if files != manifest.FileCount || descriptor.SHA256 != location.SHA256 {
		t.Fatalf("bundle files=%d descriptor=%#v", files, descriptor)
	}

	sentinel := clock.Add(-time.Hour)
	if err := os.Chtimes(service.bundlePath(manifest.WorkshopID, manifest.TreeSHA256), sentinel, sentinel); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Issue(context.Background(), "agent:node-two", manifest.WorkshopID, manifest.TreeSHA256, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(service.bundlePath(manifest.WorkshopID, manifest.TreeSHA256))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(sentinel) {
		t.Fatalf("persistent bundle was regenerated: modtime=%v", info.ModTime())
	}

	clock = clock.Add(11 * time.Minute)
	if _, file, err := service.Open(manifest.WorkshopID, manifest.TreeSHA256, token); err != ErrUnauthorized || file != nil {
		t.Fatalf("expired grant opened artifact: file=%v err=%v", file, err)
	}
}

func TestArtifactMaterializationRejectsInsufficientControllerDisk(t *testing.T) {
	service, manifest := newArtifactFixture(t)
	service.diskUsage = func(string) (*disk.UsageStat, error) {
		return &disk.UsageStat{Free: MinimumArtifactHeadroom}, nil
	}
	_, _, err := service.Issue(context.Background(), "agent:node", manifest.WorkshopID, manifest.TreeSHA256, 10*time.Minute)
	if !errors.Is(err, moddistribution.ErrInsufficientSpace) {
		t.Fatalf("insufficient disk error=%v", err)
	}
}

func TestArtifactMaterializationPrunesOldUnreferencedBundle(t *testing.T) {
	service, manifest := newArtifactFixture(t)
	clock := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return clock }
	service.diskUsage = func(string) (*disk.UsageStat, error) {
		return &disk.UsageStat{Free: 2 << 30}, nil
	}
	service.cacheLimit = bundleSpaceEstimate(manifest) + 1
	oldWorkshop, oldTree := "100", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldDirectory := filepath.Join(service.root, oldWorkshop)
	if err := os.MkdirAll(oldDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	oldBundle := service.bundlePath(oldWorkshop, oldTree)
	if err := os.WriteFile(oldBundle, []byte("old bundle"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.descriptorPath(oldWorkshop, oldTree), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := clock.Add(-DefaultBundleRetention - time.Hour)
	if err := os.Chtimes(oldBundle, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Issue(context.Background(), "agent:node", manifest.WorkshopID, manifest.TreeSHA256, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldBundle); !os.IsNotExist(err) {
		t.Fatalf("old bundle was not pruned: %v", err)
	}
}
