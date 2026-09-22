package artworkpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/entitycatalog"
)

func testPackage(t *testing.T) ([]byte, Release) {
	t.Helper()
	files := map[string][]byte{
		"catalog.json":                         []byte(`{"gameVersion":"747465","entities":3,"illustrated":2,"items":[{"id":"bundle","nameZhCN":"打包袋","image":"images/0123456789abcdef01234567.webp"},{"id":"gift","nameZhCN":"礼物","image":"images/0123456789abcdef01234567.webp"},{"id":"effect"}]}`),
		"images/0123456789abcdef01234567.webp": []byte("RIFF1234WEBPfixture"),
		"README.txt":                           []byte("fixture"),
	}
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	var total int64
	for name, value := range files {
		total += int64(len(value))
		if err := tw.WriteHeader(&tar.Header{Name: "workbench/" + name, Mode: 0600, Size: int64(len(value))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data.Bytes())
	return data.Bytes(), Release{Version: "test.1", GameVersion: "747465", SHA256: hex.EncodeToString(hash[:]), ArchiveBytes: int64(data.Len()), InstalledBytes: total, Entities: 3, Illustrated: 2, Images: 1}
}

func TestInstallSurvivesRestartAndFailureThenUninstallsOnlyResources(t *testing.T) {
	data, release := testPackage(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "resource-packs")
	save := filepath.Join(parent, "save.dat")
	if err := os.WriteFile(save, []byte("save untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(root, release, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uninstalled service must not create disk state", err)
	}
	if err = s.Import(context.Background(), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	status := s.Status()
	if status.Installed == nil || status.Installed.Bytes != release.InstalledBytes || status.Busy {
		t.Fatal(status)
	}
	image, revision, err := s.Artwork("bundle")
	if err != nil || string(image) != "RIFF1234WEBPfixture" || revision != release.SHA256 {
		t.Fatal(err, revision)
	}
	if _, _, err = s.Artwork("../save.dat"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	result, err := s.Search(entitycatalog.RuntimeSearchOptions{Query: "礼物", Limit: 1})
	if err != nil || result.Total != 1 || result.Items[0].ID != "gift" {
		t.Fatal(err, result)
	}
	result, err = s.Search(entitycatalog.RuntimeSearchOptions{Offset: 1, Limit: 1})
	if err != nil || result.Total != 3 || !result.HasMore || result.Items[0].ID != "gift" {
		t.Fatal(err, result)
	}
	for _, options := range []entitycatalog.RuntimeSearchOptions{{Limit: 121}, {Limit: 1, Offset: -1}, {Limit: 1, Query: strings.Repeat("字", 101)}} {
		if _, err = s.Search(options); !errors.Is(err, entitycatalog.ErrInvalidSearch) {
			t.Fatal(err)
		}
	}
	if err = s.Import(context.Background(), bytes.NewReader(data[:len(data)-1])); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if s.Status().Installed.Revision != revision || s.Status().Error != "invalid_package" {
		t.Fatal(s.Status())
	}
	restarted, err := New(root, release, nil)
	if err != nil || restarted.Status().Installed == nil {
		t.Fatal(err)
	}
	if _, ids := restarted.Index(); len(ids) != 2 {
		t.Fatal(ids)
	}
	if err = restarted.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = restarted.Artwork("bundle"); !errors.Is(err, ErrNotInstalled) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal(err, entries)
	}
	preserved, err := os.ReadFile(save)
	if err != nil || string(preserved) != "save untouched" {
		t.Fatal(err, string(preserved))
	}
}

func waitIdle(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.Status().Busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("package operation did not finish", s.Status())
}

func TestDownloadProgressCancellationAndSingleWriter(t *testing.T) {
	data, release := testPackage(t)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	release.Sources = map[string]string{"test": server.URL}
	s, err := New(t.TempDir(), release, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Import(context.Background(), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err = s.Download("http://untrusted.test"); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if err = s.Download("test"); err != nil {
		t.Fatal(err)
	}
	<-started
	if err = s.Download("test"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err = s.Uninstall(); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for s.Status().ReceivedBytes == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Status().ReceivedBytes == 0 || s.Status().BytesPerSecond <= 0 {
		t.Fatal(s.Status())
	}
	s.Cancel()
	waitIdle(t, s)
	if s.Status().Error != "cancelled" || s.Status().Installed == nil {
		t.Fatal(s.Status())
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.Download("test"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}

func TestDownloadAndReplaceVerifiedPackage(t *testing.T) {
	data, release := testPackage(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer server.Close()
	release.Sources = map[string]string{"test": server.URL}
	s, err := New(t.TempDir(), release, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.Download("test"); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, s)
		if s.Status().Error != "" {
			t.Fatal(s.Status())
		}
	}
	entries, err := os.ReadDir(s.root)
	if err != nil || len(entries) != 2 {
		t.Fatal("only active pointer and current pack should remain", err, entries)
	}
}

func TestExtractorRejectsTraversalLinksDuplicateFilesAndOversize(t *testing.T) {
	for _, scenario := range []string{"traversal", "symlink", "duplicate", "oversize", "unexpected"} {
		t.Run(scenario, func(t *testing.T) {
			var data bytes.Buffer
			gz := gzip.NewWriter(&data)
			tw := tar.NewWriter(gz)
			header := tar.Header{Name: "workbench/README.txt", Mode: 0600}
			switch scenario {
			case "traversal":
				header.Name = "workbench/../../save.dat"
			case "symlink":
				header.Typeflag = tar.TypeSymlink
				header.Linkname = "../../saves"
			case "oversize":
				header.Size = 65 << 10
			case "unexpected":
				header.Name = "workbench/script.lua"
			}
			if err := tw.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			if header.Size > 0 {
				tw.Write(make([]byte, header.Size))
			}
			if scenario == "duplicate" {
				tw.WriteHeader(&header)
			}
			tw.Close()
			gz.Close()
			if _, err := extract(context.Background(), bytes.NewReader(data.Bytes()), t.TempDir()); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
		})
	}
}

func TestInterruptedAndInvalidImportsKeepActivePackage(t *testing.T) {
	data, release := testPackage(t)
	s, err := New(t.TempDir(), release, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Import(context.Background(), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.Import(ctx, bytes.NewReader(data)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err = s.Artwork("bundle"); err != nil {
		t.Fatal(err)
	}
	if err = s.Import(context.Background(), io.LimitReader(strings.NewReader("invalid"), 7)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err = s.Artwork("bundle"); err != nil {
		t.Fatal(err)
	}
}
