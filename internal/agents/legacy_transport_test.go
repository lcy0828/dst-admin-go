package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dont/internal/operationprogress"
	"dont/shared"
)

func TestLegacySnapshotMapsAgentSystemMetrics(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	snapshot := legacySnapshot("agent-metrics", map[string]interface{}{
		"timestamp":      now.Unix(),
		"last_heartbeat": now.Unix(),
		"hostname":       "worker-a",
		"cpu": map[string]interface{}{
			"logical_processors": 12.0,
			"physical_cores":     6.0,
		},
		"memory": map[string]interface{}{
			"total":     float64(16 * 1024 * 1024 * 1024),
			"used":      float64(6 * 1024 * 1024 * 1024),
			"available": float64(10 * 1024 * 1024 * 1024),
		},
		"system_metrics": map[string]interface{}{
			"cpu_model":            "Test CPU",
			"cpu_usage":            24.5,
			"cpu_core_usage":       []interface{}{10.0, 39.0},
			"cpu_usage_available":  true,
			"load1":                1.25,
			"load5":                1.5,
			"load15":               2.0,
			"load_supported":       true,
			"disk_path":            "/srv/dst",
			"disk_total":           float64(1000),
			"disk_used":            float64(400),
			"disk_available":       float64(600),
			"disk_usage":           40.0,
			"disk_usage_available": true,
		},
	})

	metrics := snapshot.Metrics
	if metrics.CPUModel != "Test CPU" || metrics.CPUUsage != 24.5 || !metrics.CPUUsageAvailable {
		t.Fatalf("unexpected CPU metrics: %+v", metrics)
	}
	if len(metrics.CPUCoreUsage) != 2 || metrics.CPUCoreUsage[1] != 39 {
		t.Fatalf("unexpected per-core metrics: %+v", metrics.CPUCoreUsage)
	}
	if !metrics.LoadSupported || metrics.Load5 != 1.5 {
		t.Fatalf("unexpected load metrics: %+v", metrics)
	}
	if !metrics.DiskUsageAvailable || metrics.DiskPath != "/srv/dst" || metrics.DiskUsage != 40 {
		t.Fatalf("unexpected disk metrics: %+v", metrics)
	}
}

func TestReportCommandProgressPreservesTransferMetadata(t *testing.T) {
	var update operationprogress.Update
	ctx := operationprogress.WithReporter(context.Background(), func(value operationprogress.Update) {
		update = value
	})
	reportCommandProgress(ctx, shared.CommandProgressPayload{
		Stage: "mod.cache", Percent: 42, Message: "downloading", WorkshopID: "1392778117",
		CurrentItem: 1, TotalItems: 2, CurrentBytes: 32 << 20, TotalBytes: 92 << 20, BytesPerSecond: 4_500_375,
		Items: []shared.ModDownloadProgress{{WorkshopID: "100", Status: "succeeded"}, {WorkshopID: "1392778117", Status: "downloading"}},
	})
	if len(update.Items) != 2 || update.Items[0].Status != "succeeded" {
		t.Fatalf("lost coalesced item result: %+v", update)
	}
	if update.Stage != "mod.cache" || update.Percent != 42 || update.WorkshopID != "1392778117" ||
		update.CurrentBytes != 32<<20 || update.TotalBytes != 92<<20 || update.BytesPerSecond != 4_500_375 {
		t.Fatalf("unexpected operation progress: %#v", update)
	}
}

func TestMetricsJSONPreservesValidZeroSamples(t *testing.T) {
	payload, err := json.Marshal(Metrics{
		CPUUsageAvailable:  true,
		LoadSupported:      true,
		DiskUsageAvailable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, expected := range []string{`"cpuUsage":0`, `"load1":0`, `"load5":0`, `"load15":0`, `"diskUsage":0`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("expected %s in %s", expected, text)
		}
	}
}

func TestSystemReportTimestampDoesNotAcceptOtherPassiveReports(t *testing.T) {
	info := map[string]interface{}{"_last_system_report_at": int64(1), "_last_inventory_report_at": int64(2)}
	if legacySystemReportTimestamp(info) != 1 {
		t.Fatal("unrelated inventory or process reports must not confirm a resource refresh")
	}
	if legacySystemReportTimestamp(nil) != 0 {
		t.Fatal("missing system reports must not be treated as a fresh sample")
	}
}

func TestInventoryReportTimestampDoesNotAcceptOtherReports(t *testing.T) {
	info := map[string]interface{}{
		"_last_inventory_report_at": int64(1), "_last_system_report_at": int64(2), "timestamp": time.Now().Unix(),
	}
	if legacyInventoryReportTimestamp(info) != 1 {
		t.Fatal("resource refresh must not confirm an inventory request")
	}
	delete(info, "_last_inventory_report_at")
	if legacyInventoryReportTimestamp(info) != 0 || legacyInventoryReportTimestamp(nil) != 0 {
		t.Fatal("missing inventory receipt must not fall back to another report timestamp")
	}
}
