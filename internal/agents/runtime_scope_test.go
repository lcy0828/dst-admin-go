package agents

import (
	"errors"
	"testing"
	"time"
)

func TestRuntimeNodeScopeTracksSelectedInstallationAndProcess(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	set := func(instance, selected, other string, status Status) {
		transport.mu.Lock()
		snapshot := transport.snapshots["agent-primary"]
		snapshot.Status, snapshot.LastHeartbeat = status, time.Now()
		snapshot.Details = map[string]interface{}{
			"runtime_instance_id": instance,
			"runtime_installations": []map[string]interface{}{
				{"id": "primary", "driver": "native", "save_path": selected, "server_path": "/srv/dst/server", "server_mode": "64"},
				{"id": "other", "driver": "native", "save_path": other, "server_path": "/srv/dst/other", "server_mode": "64"},
			},
		}
		transport.snapshots[snapshot.ID] = snapshot
		transport.mu.Unlock()
		if _, err := service.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		value, err := service.RuntimeNodeScope("agent:agent-primary", "primary")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	set("boot-a", "/srv/dst/saves", "/srv/dst/other", StatusOnline)
	original := read()
	set("boot-a", "/srv/dst/saves", "/srv/dst/other-updated", StatusOnline)
	if read() != original {
		t.Fatal("heartbeat or another installation invalidated the selected scope")
	}
	set("boot-a", "/srv/dst/saves", "/srv/dst/other-updated", StatusOffline)
	if read() != original {
		t.Fatal("connection failure erased installation identity")
	}
	set("boot-b", "/srv/dst/saves", "/srv/dst/other-updated", StatusOnline)
	if read() == original {
		t.Fatal("Agent restart retained old scope")
	}
	bootB := read()
	set("boot-b", "/srv/dst/new-saves", "/srv/dst/other-updated", StatusOnline)
	if read() == bootB {
		t.Fatal("changed installation root retained old scope")
	}
	if _, err := service.RuntimeNodeScope("agent:agent-primary", "missing"); !errors.Is(err, ErrRuntimeInstallationNotRegistered) {
		t.Fatal(err)
	}
	// Changing transport metadata alone cannot trigger a Sync or collection
	// from a scope read. Only the existing event-driven sync publishes it.
	before := read()
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Version = "next-version"
	transport.snapshots[snapshot.ID] = snapshot
	transport.mu.Unlock()
	if read() != before {
		t.Fatal("scope lookup synchronized transport")
	}
	if _, err := service.Sync(); err != nil {
		t.Fatal(err)
	}
	if read() == before {
		t.Fatal("published node version did not change scope")
	}
}

func TestRuntimeNodeScopeTracksLegacyConfiguredSaveRoot(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	config := RuntimeConfig{InstallationID: "primary", SavePath: "/srv/dst/saves", ServerPath: "/srv/dst/server", ServerMode: "64"}
	if _, err := service.SaveRuntimeConfig("agent-primary", config); err != nil {
		t.Fatal(err)
	}
	first, err := service.RuntimeNodeScope("agent:agent-primary", "primary")
	if err != nil {
		t.Fatal(err)
	}
	config.SavePath = "/srv/dst/changed"
	if _, err := service.SaveRuntimeConfig("agent-primary", config); err != nil {
		t.Fatal(err)
	}
	second, err := service.RuntimeNodeScope("agent:agent-primary", "primary")
	if err != nil || first == second {
		t.Fatalf("legacy config did not change scope: %s %s %v", first, second, err)
	}
}
