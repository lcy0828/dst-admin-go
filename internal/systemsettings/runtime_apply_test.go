package systemsettings

import (
	"context"
	"errors"
	"testing"
)

func TestRuntimeApplyPersistsOnlyWhenIdleAndRollsBackFailure(t *testing.T) {
	for _, mode := range []string{"busy", "failure", "success"} {
		t.Run(mode, func(t *testing.T) {
			repo := NewMemoryRepository()
			s, _ := NewService(repo)
			before, _ := repo.Snapshot()
			called := false
			s.SetRuntimeApplier(func(_ context.Context, save, rollback func() error) error {
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
