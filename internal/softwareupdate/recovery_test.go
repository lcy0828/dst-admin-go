package softwareupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExternalUpgradeSettlesObsoleteTasksForControllerAndAgent(t *testing.T) {
	for _, kind := range []string{"", "agent"} {
		for _, phase := range []string{"prepared", "failed", "interrupted"} {
			for _, version := range []string{"v1.1.0", "v1.2.0"} {
				t.Run(kind+"/"+phase+"/"+version, func(t *testing.T) {
					s, _ := testService(t, nil)
					s.config.Kind = kind
					state, _ := readState(s.config.Root)
					id := newID()
					state.Operation = &Operation{ID: id, Version: "v1.1.0", Phase: phase, Error: "old failure"}
					if err := writeState(s.config.Root, state); err != nil {
						t.Fatal(err)
					}
					config := s.config
					config.Current.Version = version
					restored := New(config)
					snapshot, err := restored.Snapshot()
					if err != nil || snapshot.Operation.Phase != "superseded" || snapshot.Operation.ID != id || snapshot.Operation.Error != "" {
						t.Fatal("obsolete task survived external upgrade", snapshot, err)
					}
					stored, err := readState(s.config.Root)
					if err != nil || stored.Operation.Phase != "superseded" || stored.Pending != nil {
						t.Fatal(stored, err)
					}
					var resumes int
					restored.SetRestartGuard(func(context.Context) (func(), error) { return func() { resumes++ }, nil })
					if _, _, err := restored.PrepareApply(context.Background(), id); !errors.Is(err, ErrNotPrepared) || resumes != 1 {
						t.Fatal("obsolete task admitted a switch", err, resumes)
					}
				})
			}
		}
	}
}

func TestReconciliationPreservesPendingBusyFutureAndRecoveryStates(t *testing.T) {
	for _, phase := range []string{"prepared", "downloading", "verifying", "restarting", "succeeded", "rolled_back"} {
		t.Run(phase, func(t *testing.T) {
			s, _ := testService(t, nil)
			state, _ := readState(s.config.Root)
			state.Operation = &Operation{ID: newID(), Version: "v1.1.0", Phase: phase}
			if err := writeState(s.config.Root, state); err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.Snapshot()
			if err != nil || snapshot.Operation.Phase != phase {
				t.Fatal("future task changed", snapshot, err)
			}
			if phase == "prepared" {
				state.Pending = &installedRelease{Version: "v1.1.0"}
			}
			s.config.Current.Version = "v1.2.0"
			if err := writeState(s.config.Root, state); err != nil {
				t.Fatal(err)
			}
			snapshot, err = s.Snapshot()
			if err != nil || snapshot.Operation.Phase != phase {
				t.Fatal("active or recovery task changed", snapshot, err)
			}
		})
	}
	s, _ := testService(t, nil)
	s.config.Ready = func() bool { return false }
	s.config.Current.Version = "v1.2.0"
	state, _ := readState(s.config.Root)
	state.Operation = &Operation{ID: newID(), Version: "v1.1.0", Phase: "prepared"}
	if err := writeState(s.config.Root, state); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Snapshot()
	if err != nil || snapshot.Operation.Phase != "prepared" {
		t.Fatal("unverified startup settled old task", snapshot, err)
	}
}

func TestInvalidPreparedPackageCanBeDownloadedAgain(t *testing.T) {
	for _, kind := range []string{"", "agent"} {
		for _, damage := range []string{"missing", "corrupt"} {
			for _, action := range []string{"start", "apply"} {
				t.Run(kind+"/"+damage+"/"+action, func(t *testing.T) {
					data := testBundle(t, "v1.1.0", "linux-amd64", nil)
					if kind == "agent" {
						var err error
						data, err = os.ReadFile(agentFixture(t, t.TempDir(), "linux-amd64", "v1.1.0", []byte("agent")))
						if err != nil {
							t.Fatal(err)
						}
					}
					s, _ := testService(t, data)
					s.config.Kind = kind
					if _, err := s.Start("v1.1.0", "auto"); err != nil {
						t.Fatal(err)
					}
					prepared := finishDownload(t, s).Operation
					if prepared.Phase != "prepared" {
						t.Fatal(prepared)
					}
					directory, err := releaseDirectory(s.config.Root, prepared.ReleaseID)
					if err != nil {
						t.Fatal(err)
					}
					if damage == "missing" {
						err = os.RemoveAll(directory)
					} else {
						err = os.WriteFile(filepath.Join(directory, binaryFor(kind, "linux-amd64")), []byte("invalid"), 0700)
					}
					if err != nil {
						t.Fatal(err)
					}
					if action == "apply" {
						var resumes int
						s.SetRestartGuard(func(context.Context) (func(), error) { return func() { resumes++ }, nil })
						if _, _, err := s.PrepareApply(context.Background(), prepared.ID); !errors.Is(err, ErrNotPrepared) || resumes != 1 {
							t.Fatal(err, resumes)
						}
						state, err := readState(s.config.Root)
						if err != nil || state.Pending != nil || state.Operation.Phase != "failed" {
							t.Fatal("invalid package remains applicable", state, err)
						}
					}
					retry, err := s.Start("v1.1.0", "auto")
					if err != nil || retry.ID == prepared.ID {
						t.Fatal("stale prepared task returned again", retry, err)
					}
					if result := finishDownload(t, s); result.Operation.Phase != "prepared" {
						t.Fatal("retry did not prepare a valid bundle", result)
					}
				})
			}
		}
	}
}
