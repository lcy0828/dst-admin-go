package softwareupdate

import (
	"context"
	"dont/internal/buildinfo"
	"testing"
	"time"
)

func TestReleaseObserverUsesExistingCheckAndRunsOutsideSnapshotLock(t *testing.T) {
	s, client := testService(t, []byte("fixture"))
	var versions []string
	s.config.OnAvailable = func(version string) { _, _ = s.Snapshot(); versions = append(versions, version) }
	if _, err := s.Check(context.Background(), true, "direct"); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0] != "v1.1.0" {
		t.Fatal("new release event missing", versions)
	}
	if _, err := s.Check(context.Background(), false, "direct"); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || client.calls.Load() != 1 {
		t.Fatal("notification forced an extra check", versions, client.calls.Load())
	}
}

func TestOperationObserverRunsAfterUnlockAndRequiresPersistedResult(t *testing.T) {
	// The callback may read ordinary service state without deadlocking Snapshot.
	s := New(Config{Root: t.TempDir(), Current: buildinfo.Info{Version: "v1.0.0"}, Platform: "linux-amd64", Managed: true, Ready: func() bool { return true }})
	observed := 0
	s.config.OnOperation = func(_ string, op Operation, healthy bool) {
		s.mu.Lock()
		s.mu.Unlock()
		if op.Phase == "succeeded" && healthy {
			observed++
		}
	}
	now := time.Now().UTC()
	if err := writeState(s.config.Root, diskState{Protocol: Protocol, Operation: &Operation{ID: "1234567890abcdef1234567890abcdef", Version: "v1.0.0", Phase: "succeeded", StartedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if observed != 1 {
		t.Fatal("durable terminal result missing")
	}
}
