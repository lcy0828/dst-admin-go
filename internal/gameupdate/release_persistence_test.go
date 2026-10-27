package gameupdate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinzhu/gorm"
)

func TestReleasePrewritePersistenceFailureRetainsEvidenceAndRestoresWorlds(t *testing.T) {
	for _, phase := range []string{"protecting", "staged", "dispatched"} {
		t.Run(phase, func(t *testing.T) {
			f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
			fired := false
			injected := errors.New("injected release persistence error")
			f.coordinator.store.db.Callback().Update().Before("gorm:update").Register("test:fail_release_save", func(scope *gorm.Scope) {
				if fired || scope.TableName() != f.coordinator.store.releaseTable {
					return
				}
				attrs, _ := scope.InstanceGet("gorm:update_attrs")
				values, _ := attrs.(map[string]interface{})
				match := values["stage"] == phase
				if phase == "dispatched" {
					evidence, _ := values["execution_json"].(string)
					match = strings.Contains(evidence, `"writeState":"dispatched"`)
				}
				if match {
					fired = true
					scope.Err(injected)
				}
			})
			value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "save-" + phase, Plan: f.plan})
			if !fired || !errors.Is(err, injected) || value.ID == "" || value.Stage != ReleaseStageFailed {
				t.Fatalf("fired=%v stage=%s id=%s err=%v", fired, value.Stage, value.ID, err)
			}
			if len(f.runtime.updateCounts) != 0 || value.WritingStarted {
				t.Fatal("write dispatched after persistence failure")
			}
			for key, state := range f.runtime.states {
				if key != "room-b\x00Archive" && state.State != "running" {
					t.Fatalf("world not restored: %s", key)
				}
			}
		})
	}
}

func TestReleaseRebasePersistsPlanAndExecutionInOneTransaction(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "atomic-rebase", Plan: f.plan})
	if err != nil {
		t.Fatal(err)
	}
	previousHash := value.Plan.PlanHash
	value.PlanHistory = append(value.PlanHistory, value.Plan)
	value.Plan.DesiredVersion = "702"
	for i := range value.Plan.Installations {
		value.Plan.Installations[i].DesiredVersion = "702"
	}
	value.Plan.PlanHash, err = calculateReleasePlanHash(value.Plan)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected child evidence persistence failure")
	f.coordinator.store.db.Callback().Update().Before("gorm:update").Register("test:fail_rebase_child", func(scope *gorm.Scope) {
		if scope.TableName() == f.coordinator.store.installationTable {
			scope.Err(injected)
		}
	})
	if _, err = f.coordinator.store.Rebase(value, previousHash); !errors.Is(err, injected) {
		t.Fatalf("missing error: %v", err)
	}
	stored, err := f.coordinator.Get(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Plan.PlanHash != previousHash || len(stored.PlanHistory) != 0 {
		t.Fatal("failed rebase partially persisted new plan or execution evidence")
	}
}
