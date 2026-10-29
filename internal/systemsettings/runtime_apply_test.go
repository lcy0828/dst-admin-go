package systemsettings

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestRuntimeApplyPersistsOnlyWhenIdleAndRollsBackFailure(t *testing.T) {
	for _, mode := range []string{"busy", "failure", "success"} {
		t.Run(mode, func(t *testing.T) {
			repo := NewMemoryRepository()
			s, _ := NewService(repo)
			before, _ := repo.Snapshot()
			called := false
			s.SetRuntimeApplier(func(_ context.Context, _ []string, save, rollback func() error) error {
				called = true
				if mode == "busy" {
					return ErrRuntimeBusy
				}
				if err := save(); err != nil {
					return err
				}
				if mode == "failure" {
					return errors.Join(errors.New("invalid runtime"), rollback())
				}
				return nil
			})
			input := Input{Revision: before.Revision, Confirmation: ApplyConfirmation, Values: map[string]string{"paths.save": "/opt/dst/new-saves"}}
			preview, err := s.Preview(input)
			if err != nil || preview.RestartRequired {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			result, err := s.Apply(input)
			if !called {
				t.Fatal("runtime applier was not invoked")
			}
			after, _ := repo.Snapshot()
			if mode == "success" {
				if err != nil || result.Settings.RestartRequired || after.Values["paths.save"] != input.Values["paths.save"] {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if err == nil || after.Values["paths.save"] != before.Values["paths.save"] {
				t.Fatalf("failed apply changed settings: %v", err)
			}
		})
	}
}

func TestSavingExplicitDefaultsDoesNotRequireRestart(t *testing.T) {
	s, _ := NewService(NewMemoryRepository())
	before, _ := s.Settings()
	result, err := s.Apply(Input{Revision: before.Revision, Confirmation: ApplyConfirmation, Values: map[string]string{"fleet.localExecutorEnabled": "true", "fleet.controllerEnabled": "true", "fleet.memberEnabled": "false"}})
	if err != nil || result.Settings.RestartRequired {
		t.Fatalf("equivalent defaults require restart: %v", err)
	}
}

func TestRuntimeApplyUsesActiveValuesAndIncludesPendingFileChanges(t *testing.T) {
	repo := NewMemoryRepository()
	s, err := NewService(repo)
	if err != nil {
		t.Fatal(err)
	}
	s.lookupEnv = func(string) (string, bool) { return "", false }
	var changed []string
	s.SetRuntimeApplier(func(_ context.Context, fields []string, save, _ func() error) error {
		changed = append([]string(nil), fields...)
		return save()
	})
	before, _ := s.Settings()
	result, err := s.Apply(Input{Revision: before.Revision, Confirmation: ApplyConfirmation, Values: map[string]string{
		"fleet.localExecutorEnabled": "true", "fleet.controllerEnabled": "false", "fleet.memberEnabled": "false",
		"ui.systemName": "Online role switch",
	}})
	if err != nil || !slices.Equal(changed, []string{"fleet.controllerEnabled"}) || result.Settings.RestartRequired {
		t.Fatalf("role-only apply: changed=%v result=%+v err=%v", changed, result, err)
	}
	// An operator edited the file after this generation started. A subsequent
	// role save must not bypass the stop requirement for that pending path.
	snapshot, err := repo.Save(result.Settings.Revision, map[string]string{"paths.save": "/opt/dst/pending-saves"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(Input{Revision: snapshot.Revision, Confirmation: ApplyConfirmation, Values: map[string]string{"fleet.controllerEnabled": "true"}})
	if err != nil || !slices.Equal(changed, []string{"paths.save", "fleet.controllerEnabled"}) {
		t.Fatalf("pending file changes were omitted: changed=%v err=%v", changed, err)
	}
}
