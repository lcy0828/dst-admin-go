package tmux

import (
	"path/filepath"
	"reflect"
	"testing"

	"dont/shared"
)

func TestRuntimeModeArguments(t *testing.T) {
	tests := []struct {
		mode shared.RuntimePerformanceMode
		want []string
	}{
		{shared.RuntimePerformanceModeGame, []string{"-lua_vm_type=game"}},
		{shared.RuntimePerformanceModeLuaJIT, []string{"-lua_vm_type=jit"}},
		{shared.RuntimePerformanceModeArenaGC, []string{"-lua_vm_type=jit_gen"}},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			got, err := runtimeModeArguments(test.mode)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("arguments=%v err=%v", got, err)
			}
		})
	}
	for _, removed := range []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeJITOff, shared.RuntimePerformanceModeJITOn} {
		if _, err := runtimeModeArguments(removed); err == nil {
			t.Fatal("removed JIT override accepted")
		}
	}
	if _, err := runtimeModeArguments("future-mode"); err == nil {
		t.Fatal("unknown runtime mode was accepted")
	}
}

func TestWorkshopStateIsPrivateToEachWorld(t *testing.T) {
	root := t.TempDir()
	master := &DSTServer{StorageRoot: root, ConfDir: "saves", ArchiveName: "room", WorldName: "Master", UGCDirectory: filepath.Join(root, "steamcmd")}
	caves := &DSTServer{StorageRoot: root, ConfDir: "saves", ArchiveName: "room", WorldName: "Caves"}
	other := &DSTServer{StorageRoot: root, ConfDir: "saves", ArchiveName: "other", WorldName: "Master"}
	seen := map[string]bool{master.UGCDirectory: true}
	for _, server := range []*DSTServer{master, caves, other} {
		path := server.workshopStateDirectory()
		if seen[path] {
			t.Fatalf("shared Steam state directory: %s", path)
		}
		seen[path] = true
	}
}

func TestRuntimeLaunchArgumentsAlwaysUseLocalMods(t *testing.T) {
	if values := runtimeLaunchArguments(shared.RuntimeLaunchOptions{}); !reflect.DeepEqual(values, []string{"-skip_update_server_mods"}) {
		t.Fatalf("default launch arguments = %#v", values)
	}
	values := runtimeLaunchArguments(shared.RuntimeLaunchOptions{SkipUpdateServerMods: true})
	if len(values) != 1 || values[0] != "-skip_update_server_mods" {
		t.Fatalf("converged launch arguments = %#v", values)
	}
}
