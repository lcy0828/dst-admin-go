package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/dstserver"
	"dont/internal/moddistribution"
	"dont/shared"
)

func TestModLocalEntryUsesActualMacAppContentDirectory(t *testing.T) {
	_, installation := newModOperationAgent(t)
	contentRoot := filepath.Join(installation.ServerPath, "dontstarve_steam.app", "Contents")
	executable := filepath.Join(contentRoot, "MacOS", dstserver.BinaryX64)
	if err := os.MkdirAll(filepath.Dir(executable), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0o750); err != nil {
		t.Fatal(err)
	}
	server := runtimeModServerPath(installation)
	if server != contentRoot {
		t.Fatalf("Mod content path = %q, want %q", server, contentRoot)
	}
	if err := (&localDownloadFixture{}).Download(context.Background(), installation, []string{"123"}); err != nil {
		t.Fatal(err)
	}
	if err := moddistribution.LinkWorkshopMods(context.Background(), server, installation.WorkshopContentPath, []string{"123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(contentRoot, "mods", "workshop-123", "modinfo.lua")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(installation.ServerPath, "mods")); !os.IsNotExist(err) {
		t.Fatalf("created Mod entry outside app content: %v", err)
	}
}

type localDownloadFixture struct {
	calls int
	err   error
}

func (r *localDownloadFixture) Download(_ context.Context, installation RuntimeInstallation, ids []string) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	for _, id := range ids {
		root := filepath.Join(installation.WorkshopContentPath, id)
		if err := os.MkdirAll(root, 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "modinfo.lua"), []byte("name=\"Test\"\nversion=\"1\"\n"), 0o640); err != nil {
			return err
		}
	}
	return nil
}

func (*localDownloadFixture) Close() error { return nil }

func TestDirectDownloadLinksContentAndCachedLinkDoesNotDownload(t *testing.T) {
	a, installation := newModOperationAgent(t)
	runner := &localDownloadFixture{}
	a.modDownloadRunner = runner
	sequence := 0
	for _, action := range []shared.RuntimeAction{shared.RuntimeActionModDownload, shared.RuntimeActionModLink} {
		result, err := executeModRequest(t, a, &sequence, action, shared.RuntimeModRequest{WorkshopIDs: []string{"123"}})
		if err != nil || !result.Complete {
			t.Fatalf("action=%s result=%#v err=%v", action, result, err)
		}
	}
	if runner.calls != 1 {
		t.Fatalf("cached linking downloaded again: %d", runner.calls)
	}
	if target, err := os.Readlink(filepath.Join(installation.ServerPath, "mods", "workshop-123")); err != nil || target != filepath.Join(installation.WorkshopContentPath, "123") {
		t.Fatalf("local link=%q err=%v", target, err)
	}
	entries, err := os.ReadDir(installation.SavePath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Mod action changed world files: %#v err=%v", entries, err)
	}
}

func TestFailedDirectDownloadDoesNotReportLocalModReady(t *testing.T) {
	a, installation := newModOperationAgent(t)
	a.modDownloadRunner = &localDownloadFixture{err: errors.New("SteamCMD I/O Operation Failed")}
	sequence := 0
	result, err := executeModRequest(t, a, &sequence, shared.RuntimeActionModDownload, shared.RuntimeModRequest{WorkshopIDs: []string{"123"}})
	if err == nil || result.Complete {
		t.Fatalf("failed download was reported ready: %#v err=%v", result, err)
	}
	if _, err := os.Lstat(filepath.Join(installation.ServerPath, "mods", "workshop-123")); !os.IsNotExist(err) {
		t.Fatalf("failed download exposed content: %v", err)
	}
}
