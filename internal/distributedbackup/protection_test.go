package distributedbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyProtectionChecksOwnershipAndArtifactsIncludingUnstartedRooms(t *testing.T) {
	for _, configurationOnly := range []bool{false, true} {
		fixture := newDistributedBackupFixture(t)
		if configurationOnly {
			for _, world := range []struct{ root, shard string }{{fixture.masterRoot, "Master"}, {fixture.cavesRoot, "Caves"}} {
				if err := os.RemoveAll(filepath.Join(world.root, "Cluster_1", world.shard, "save")); err != nil {
					t.Fatal(err)
				}
			}
		}
		value, err := fixture.coordinator.Create(context.Background(), "room", "protection", "protection", "release-job")
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.coordinator.VerifyProtection(context.Background(), value.ID, "room", "release-job"); err != nil {
			t.Fatalf("valid protection (configurationOnly=%v): %v", configurationOnly, err)
		}
		for _, owner := range []struct{ room, job string }{{"another-room", "release-job"}, {"room", "another-job"}} {
			if err := fixture.coordinator.VerifyProtection(context.Background(), value.ID, owner.room, owner.job); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("foreign protection accepted: %v", err)
			}
		}
		path, _ := fixture.coordinator.partPath(value.Parts[0])
		if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := fixture.coordinator.VerifyProtection(context.Background(), value.ID, "room", "release-job"); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("corruption accepted: %v", err)
		}
	}
}
