package gameupdate

import (
	"fmt"
	"testing"
	"time"
)

func TestPendingReleasesIncludeOldRecoveryAndPaginateBeforeHistoryLimit(t *testing.T) {
	store := NewReleaseStore(openGameUpdateTestDB(t), "pending_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	plan := readyReleasePlanForStore(t)
	now := time.Now().UTC()
	for i := 0; i < 65; i++ {
		stage := ReleaseStageSucceeded
		if i == 0 {
			stage = ReleaseStageRecoveryRequired
		}
		if i == 1 {
			stage = ReleaseStageUpdating
		}
		if i == 2 {
			stage = ReleaseStageFailed
		}
		if i == 3 {
			stage = ReleaseStageAbandoned
		}
		at := now.Add(time.Duration(i) * time.Second)
		value := Release{ID: fmt.Sprintf("release-%03d", i), Stage: stage, Plan: plan, SourceJobID: fmt.Sprintf("job-%03d", i), CreatedAt: at, UpdatedAt: at}
		for _, target := range plan.Installations {
			value.Installations = append(value.Installations, ReleaseInstallationResult{TargetID: target.TargetID, InstallationID: target.InstallationID, Stage: stage, UpdatedAt: at})
			for _, shard := range target.Shards {
				value.Shards = append(value.Shards, ReleaseShardResult{RoomID: shard.RoomID, WorldID: shard.WorldID, TargetID: shard.TargetID, InstallationID: shard.InstallationID, IsMaster: shard.IsMaster, WasRunning: shard.WasRunning, Stage: stage, UpdatedAt: at})
			}
		}
		if _, err := store.Create(value); err != nil {
			t.Fatal(err)
		}
	}
	recent, total, err := store.List(50, 0)
	if err != nil || total != 65 || len(recent) != 50 {
		t.Fatal(total, len(recent), err)
	}
	for _, item := range recent {
		if item.Stage == ReleaseStageRecoveryRequired {
			t.Fatal("fixture recovery should be older than recent history")
		}
	}
	first, total, err := store.list(1, 0, true)
	if err != nil || total != 2 || len(first) != 1 || first[0].Stage != ReleaseStageUpdating {
		t.Fatal(first, total, err)
	}
	second, total, err := store.list(1, 1, true)
	if err != nil || total != 2 || len(second) != 1 || second[0].Stage != ReleaseStageRecoveryRequired {
		t.Fatal(second, total, err)
	}
}
