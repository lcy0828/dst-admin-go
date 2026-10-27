package gameupdate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/runtimedriver"
	"dont/shared"
)

func TestNativeReleaseJournalSurvivesServiceReplacementAndDoesNotResend(t *testing.T) {
	service, _, _, _, events, _ := newUpdateService(t, nil)
	executable := filepath.Join(t.TempDir(), "steamcmd")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0750); err != nil {
		t.Fatal(err)
	}
	service.config.SteamCMDPath = executable
	operation := runtimedriver.Operation{ID: "persistent-native-operation"}
	result, err := service.UpdateReleaseInstallationOperation(context.Background(), operation, "200", false)
	if err != nil || result.CurrentVersion != "200" {
		t.Fatalf("update=%+v error=%v", result, err)
	}
	// A new service object has no in-memory operation state.
	replacement := &Service{config: service.config, runner: service.runner, now: service.now}
	evidence, err := replacement.ObserveReleaseUpdate(context.Background(), operation.ID)
	if err != nil || !evidence.Completed || evidence.Outcome != shared.RuntimeOutcomeConfirmed {
		t.Fatalf("evidence=%+v error=%v", evidence, err)
	}
	before := len(*events)
	if _, err = replacement.UpdateReleaseInstallationOperation(context.Background(), operation, "200", false); err != nil {
		t.Fatal(err)
	}
	if len(*events) != before {
		t.Fatal("persistent operation was resent")
	}
	evidence.Completed = false
	if err = service.writeReleaseEvidence(evidence); err != nil {
		t.Fatal(err)
	}
	if _, err = replacement.UpdateReleaseInstallationOperation(context.Background(), operation, "200", false); err == nil {
		t.Fatal("unknown operation treated as successful")
	}
	if len(*events) != before {
		t.Fatal("unknown write was implicitly resent")
	}
}
