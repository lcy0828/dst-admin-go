package modcontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/shared"
)

type parallelModFetcher struct {
	mu                     sync.Mutex
	active, maximum, calls int
	started                chan string
	proceed                chan struct{}
	fail                   string
	modIDsByInstallation   map[string][]string
}

func (f *parallelModFetcher) LinkInstallationMods(context.Context, string, string, []string) error {
	return nil
}

func (f *parallelModFetcher) UpdateInstallationMods(ctx context.Context, targetID, installationID string, ids []string, out io.Writer) (mods.ActionResult, error) {
	f.mu.Lock()
	if f.modIDsByInstallation == nil {
		f.modIDsByInstallation = make(map[string][]string)
	}
	f.modIDsByInstallation[targetID+"/"+installationID] = append([]string(nil), ids...)
	f.active++
	f.calls++
	f.maximum = max(f.maximum, f.active)
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	if f.started != nil {
		f.started <- targetID
	}
	if f.proceed != nil {
		select {
		case <-f.proceed:
		case <-ctx.Done():
			return mods.ActionResult{}, ctx.Err()
		}
	}
	items := []shared.ModDownloadProgress{{WorkshopID: "100", TargetID: targetID, InstallationID: installationID, Status: "downloading", CurrentBytes: 40, TotalBytes: 100}}
	operationprogress.Report(ctx, operationprogress.Update{Percent: 70, WorkshopID: "100", CurrentItem: 1, TotalItems: 1, CurrentBytes: 40, TotalBytes: 100, BytesPerSecond: 42, Items: append([]shared.ModDownloadProgress(nil), items...)})
	operationprogress.Report(ctx, operationprogress.Update{Percent: 20})
	_, _ = fmt.Fprintln(out, targetID+"/"+installationID)
	if targetID == f.fail {
		items[0].Status = "failed"
		operationprogress.Report(ctx, operationprogress.Update{Percent: 70, Items: items})
		return mods.ActionResult{}, errors.New("SteamCMD I/O Operation Failed")
	}
	items[0].Status = "succeeded"
	operationprogress.Report(ctx, operationprogress.Update{Percent: 100, Items: items})
	return mods.ActionResult{}, nil
}

func TestInstallationDownloadsOverlapWithBoundedWorkersAndMonotonicProgress(t *testing.T) {
	fetcher := &parallelModFetcher{started: make(chan string, 8), proceed: make(chan struct{})}
	service := &Service{installer: fetcher}
	targets := make(map[string]modpublication.AppliedPlacement)
	for index := 0; index < 8; index++ {
		id := fmt.Sprintf("agent:node-%d", index)
		targets[id] = modpublication.AppliedPlacement{TargetID: id, InstallationID: "native"}
	}
	var output bytes.Buffer
	progress := 0
	var latest []shared.ModDownloadProgress
	ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) {
		latest = update.Items
		if len(latest) != 8 {
			t.Errorf("lost machine progress: %+v", update.Items)
		}
		if update.WorkshopID == "100" && (update.CurrentItem != 1 || update.TotalItems != 1 || update.CurrentBytes != 40 || update.TotalBytes != 100 || update.TargetID == "" || update.InstallationID != "native") {
			t.Errorf("per-mod progress lost in node aggregation: %+v", update)
		}
		if update.Percent < progress || update.Stage == operationprogress.StageModDone {
			t.Errorf("invalid aggregate progress: %#v", update)
		}
		progress = update.Percent
	})
	done := make(chan error, 1)
	go func() {
		downloaded, ready, err := service.downloadRoomInstallations(ctx, targets, []string{"100"}, &output)
		if downloaded != 8 || ready != 0 {
			err = fmt.Errorf("downloaded=%d ready=%d err=%v", downloaded, ready, err)
		}
		done <- err
	}()
	for index := 0; index < installationDownloadConcurrency; index++ {
		select {
		case <-fetcher.started:
		case <-time.After(2 * time.Second):
			close(fetcher.proceed)
			t.Fatal("downloads remained serial")
		}
	}
	close(fetcher.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fetcher.maximum != installationDownloadConcurrency || fetcher.calls != 8 || progress != 100 || strings.Count(output.String(), "\n") != 8 {
		t.Fatalf("fetcher=%#v progress=%d output=%q", fetcher, progress, output.String())
	}
	for _, item := range latest {
		if item.Status != "succeeded" || item.TargetID == "" {
			t.Fatalf("lost completed machine result: %+v", latest)
		}
	}
}

func TestInstallationPartialDownloadFailureNamesTargetAndDoesNotPublish(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fetcher := &parallelModFetcher{fail: "target-2"}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	service.installer = fetcher
	publisher := &modConfigurationPublisherFixture{}
	service.configuration = publisher
	_, err = service.Install(context.Background(), "job", fixture.room.ID, mods.InstallRequest{
		ModID: "200", WorldIDs: []string{fixture.master.ID, fixture.caves.ID}, Enabled: true,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "target-2/install-2") || !strings.Contains(err.Error(), "1/2") {
		t.Fatalf("failure missing target results: %v", err)
	}
	if len(publisher.writes) != 0 || fetcher.calls != 2 {
		t.Fatalf("partial download published configuration: %#v calls=%d", publisher, fetcher.calls)
	}
}
