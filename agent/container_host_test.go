package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"dont/internal/shards"
	"dont/shared"
)

func containerHostTestConfig() ContainerRuntimeHostConfig {
	return ContainerRuntimeHostConfig{
		Installation:   containerTestInstallation(),
		Image:          "dst-admin/dst-runtime:test",
		HostSavePath:   "/opt/dst/saves",
		HostServerPath: "/opt/dst/server",
		HostUGCPath:    "/opt/dst/workshop/steamapps/workshop",
		Timezone:       "Asia/Shanghai",
	}
}

func TestContainerRuntimeHostTreatsMissingContainerAsStopped(t *testing.T) {
	cli := &fakeContainerCLI{available: true, responses: [][]byte{nil}}
	host, err := newContainerRuntimeHost(containerHostTestConfig(), cli)
	if err != nil {
		t.Fatal(err)
	}
	status, err := host.Status(context.Background(), "Cluster_1", "Master")
	if err != nil || status.State != shards.RuntimeStopped || status.Code != "CONTAINER_NOT_CREATED" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestContainerRuntimeHostCreatesMissingShardFromTrustedConfig(t *testing.T) {
	cli := &fakeContainerCLI{available: true, responses: [][]byte{nil, []byte(strings.Repeat("a", 64) + "\n")}}
	host, err := newContainerRuntimeHost(containerHostTestConfig(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.ensureContainer(context.Background(), "Cluster_1", "Caves"); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 2 || len(cli.calls[1].arguments) == 0 || cli.calls[1].arguments[0] != "create" {
		t.Fatalf("calls=%#v", cli.calls)
	}
	arguments := cli.calls[1].arguments
	for _, required := range []string{
		"com.dst-admin.installation=runtime-a",
		"com.dst-admin.cluster=Cluster_1",
		"com.dst-admin.shard=Caves",
		"type=bind,src=/opt/dst/saves,dst=/opt/dst/saves",
		"type=bind,src=/opt/dst/server,dst=/opt/dst/server,readonly",
		"type=bind,src=/opt/dst/workshop/steamapps/workshop,dst=/opt/dst/workshop/steamapps/workshop,readonly",
		"DST_CLUSTER=Cluster_1",
		"DST_SHARD=Caves",
		"DST_STORAGE_ROOT=/opt/dst/saves",
		"DST_UGC_DIRECTORY=/opt/dst/saves/.dst-admin/runtime/workshop/Cluster_1/Caves",
		"com.dst-admin.local-mods=true",
		"dst-admin/dst-runtime:test",
	} {
		if !containsContainerArgument(arguments, required) {
			t.Fatalf("missing argument %q in %#v", required, arguments)
		}
	}
	for _, forbidden := range []string{"sh", "bash", "-c", "--privileged"} {
		if containsContainerArgument(arguments, forbidden) {
			t.Fatalf("unsafe argument %q in %#v", forbidden, arguments)
		}
	}
}

func TestContainerRuntimeHostDoesNotRecreateExistingShard(t *testing.T) {
	id := strings.Repeat("b", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{containerListLine(id, "exited", "Cluster_1", "Master")}}
	host, err := newContainerRuntimeHost(containerHostTestConfig(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.ensureContainer(context.Background(), "Cluster_1", "Master"); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 1 || !reflect.DeepEqual(cli.calls[0].arguments[:2], []string{"ps", "-a"}) {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerLocalModsMountConfiguredLinkTargetReadOnly(t *testing.T) {
	config := containerHostTestConfig()
	config.Installation.WorkshopContentPath = "/custom/steamapps/workshop/content/322330"
	cli := &fakeContainerCLI{available: true, responses: [][]byte{nil, []byte(strings.Repeat("a", 64))}}
	host, err := newContainerRuntimeHost(config, cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.ensureContainer(context.Background(), "Cluster_1", "Caves"); err != nil {
		t.Fatal(err)
	}
	expected := "type=bind,src=/opt/dst/workshop/steamapps/workshop/content/322330,dst=/custom/steamapps/workshop/content/322330,readonly"
	if !containsContainerArgument(cli.calls[1].arguments, expected) {
		t.Fatalf("missing read-only link target mount: %v", cli.calls)
	}
}

func TestContainerLocalModUpgradeNeverRecreatesRunningWorld(t *testing.T) {
	id := strings.Repeat("b", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{containerListLine(id, "running", "Cluster_1", "Master")}}
	host, err := newContainerRuntimeHost(containerHostTestConfig(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.ensureContainerForRuntimeMode(context.Background(), "Cluster_1", "Master", shared.RuntimePerformanceModeGame, true); err == nil {
		t.Fatal("running legacy container accepted a layout replacement")
	}
	if len(cli.calls) != 1 {
		t.Fatalf("running container was changed: %#v", cli.calls)
	}
}

func TestContainerLocalModUpgradeRecreatesOnlyStoppedContainer(t *testing.T) {
	id := strings.Repeat("b", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{containerListLine(id, "exited", "Cluster_1", "Master"), nil, []byte(id)}}
	host, err := newContainerRuntimeHost(containerHostTestConfig(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.ensureContainerForRuntimeMode(context.Background(), "Cluster_1", "Master", shared.RuntimePerformanceModeGame, true); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 3 || cli.calls[1].arguments[0] != "rm" || cli.calls[2].arguments[0] != "create" {
		t.Fatalf("stopped container layout was not replaced: %#v", cli.calls)
	}
}

func TestContainerRuntimeHostRejectsUnsafeMaterializationConfig(t *testing.T) {
	tests := []ContainerRuntimeHostConfig{
		func() ContainerRuntimeHostConfig { value := containerHostTestConfig(); value.Image = ""; return value }(),
		func() ContainerRuntimeHostConfig {
			value := containerHostTestConfig()
			value.HostSavePath = "/"
			return value
		}(),
		func() ContainerRuntimeHostConfig {
			value := containerHostTestConfig()
			value.HostServerPath = "/opt/dst/saves/server"
			return value
		}(),
		func() ContainerRuntimeHostConfig {
			value := containerHostTestConfig()
			value.HostUGCPath = "/opt/dst/server/workshop"
			return value
		}(),
	}
	for _, config := range tests {
		if _, err := newContainerRuntimeHost(config, &fakeContainerCLI{available: true}); err == nil {
			t.Fatalf("config unexpectedly accepted: %#v", config)
		}
	}
}

func TestContainerRuntimeHostCPUPrepareCreatesFirstShardBeforeLifecycleStart(t *testing.T) {
	id := strings.Repeat("c", 64)
	config := containerHostTestConfig()
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		nil,
		[]byte(id + "\n"),
		containerListLine(id, "created", "Cluster_1", "Master"),
		nil,
		containerListLine(id, "created", "Cluster_1", "Master"),
		containerCPUInspect(id, "Cluster_1", "Master", "", 0, false),
	}}
	host, err := newContainerRuntimeHost(config, cli)
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Prepare(context.Background(), config.Installation.ID, "Cluster_1", "Master", shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyNone})
	if err != nil || result.State != shared.RuntimeCPUStateReleased {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(cli.calls) != 6 || cli.calls[1].arguments[0] != "create" || cli.calls[3].arguments[0] != "update" {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeHostCleanupRemovesStoppedShard(t *testing.T) {
	id := strings.Repeat("d", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "exited", "Cluster_1", "Master"), nil,
	}}
	host, err := newContainerRuntimeHost(containerHostTestConfig(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Cleanup(context.Background(), "Cluster_1", "Master"); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 2 || !reflect.DeepEqual(cli.calls[1].arguments, []string{"rm", "--force", id}) {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func TestContainerRuntimeHostCleanupRejectsRunningShard(t *testing.T) {
	id := strings.Repeat("e", 64)
	cli := &fakeContainerCLI{available: true, responses: [][]byte{
		containerListLine(id, "running", "Cluster_1", "Master"),
	}}
	host, err := newContainerRuntimeHost(containerHostTestConfig(), cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Cleanup(context.Background(), "Cluster_1", "Master"); err == nil {
		t.Fatal("running container cleanup unexpectedly succeeded")
	}
	if len(cli.calls) != 1 {
		t.Fatalf("calls=%#v", cli.calls)
	}
}

func containsContainerArgument(arguments []string, expected string) bool {
	for _, argument := range arguments {
		if argument == expected {
			return true
		}
	}
	return false
}
