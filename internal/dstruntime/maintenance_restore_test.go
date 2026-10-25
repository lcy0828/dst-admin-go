package dstruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dont/shared"
)

func TestMaintenanceFailurePreservesUserEditOutsideManagedBlock(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	world := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, world)
	installPublishedRuntimeFixture(t, manager, catalog, world)
	if err := control.state.DoString(managedActivationScript); err != nil {
		t.Fatal(err)
	}
	control.delayLoads = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	edited := false
	control.afterSend = func(script string) {
		if !strings.Contains(script, "local __update=") {
			return
		}
		path := filepath.Join(world, customCommandsName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = append([]byte("user_prefix_during_update = true\n"), data...)
		if err := os.WriteFile(path, append(data, []byte("\nuser_setting_during_update = true\n")...), 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		edited = true
		cancel()
	}
	result, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrManagedFileChanged) || !edited || result.Version != "2.4.11" {
		t.Fatalf("did not exercise failed activation and manual edit: %+v %v", result, err)
	}
	data, readErr := os.ReadFile(filepath.Join(world, customCommandsName))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "user_setting_during_update = true") || !strings.HasPrefix(string(data), "user_prefix_during_update = true\n") || !strings.Contains(string(data), "version=2.4.11") {
		t.Fatalf("automatic failure recovery silently erased the user's edit outside managed block: result=%+v err=%v customcommands=%s", result, err, data)
	}
	if info, err := os.Stat(filepath.Join(world, customCommandsName)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("recovery changed user permissions: %v %v", info, err)
	}
}

func TestMaintenanceRecoveryLeavesConcurrentManagedEditsUntouched(t *testing.T) {
	for _, target := range []string{"loader edited", "loader removed", "loader deleted", "canonical asset", "execution asset", "manifest"} {
		t.Run(target, func(t *testing.T) {
			manager, catalog, root := newRuntimeTestManager(t)
			world := filepath.Join(root, "Cluster_1", "Master")
			control := newMaintenanceLuaControl(t, world)
			installPublishedRuntimeFixture(t, manager, catalog, world)
			if err := control.state.DoString(managedActivationScript); err != nil {
				t.Fatal(err)
			}
			control.delayLoads = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var changedPath string
			var changed []byte
			control.afterSend = func(script string) {
				if !strings.Contains(script, "local __update=") {
					return
				}
				changedPath = filepath.Join(world, customCommandsName)
				switch target {
				case "canonical asset":
					changedPath = filepath.Join(world, managedDirectory, "telemetry.lua")
				case "execution asset":
					changedPath = filepath.Join(world, executionDirectory, "telemetry.lua")
				case "manifest":
					changedPath = filepath.Join(world, managedDirectory, manifestName)
				}
				var err error
				changed, err = os.ReadFile(changedPath)
				if err != nil {
					t.Fatal(err)
				}
				switch target {
				case "loader edited":
					changed = []byte(strings.Replace(string(changed), "local chunk, compile_error", "-- manual managed edit\n        local chunk, compile_error", 1))
				case "loader removed":
					changed, _, err = removeManagedBlock(changed)
					if err != nil {
						t.Fatal(err)
					}
				case "loader deleted":
					if err := os.Remove(changedPath); err != nil {
						t.Fatal(err)
					}
					cancel()
					return
				case "manifest":
					changed = append(changed, '\n')
				default:
					changed = append(changed, []byte("\n-- manual asset edit\n")...)
				}
				if err := os.WriteFile(changedPath, changed, 0640); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			result, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
			if !errors.Is(err, ErrManagedFileChanged) || result.Deferred {
				t.Fatalf("concurrent managed edit was not a recovery conflict: %+v %v", result, err)
			}
			if target == "loader deleted" {
				if _, err := os.Stat(changedPath); !os.IsNotExist(err) {
					t.Fatalf("recovery restored explicitly deleted loader: %v", err)
				}
			} else if data, err := os.ReadFile(changedPath); err != nil || string(data) != string(changed) {
				t.Fatalf("recovery overwrote edited file %s: %s %v", changedPath, data, err)
			}
			installed, _, err := readManifest(filepath.Join(world, managedDirectory, manifestName))
			if err != nil || installed.Version != RuntimeVersion {
				t.Fatalf("conflicting recovery partially restored files: %+v %v", installed, err)
			}
		})
	}
}

func TestFailedFirstRuntimeInstallationKeepsNewUserScriptAndSaves(t *testing.T) {
	for _, original := range []string{"", "existing_user_setting = true\n"} {
		t.Run("existing_script="+original, func(t *testing.T) {
			_, _, root := newRuntimeTestManager(t)
			world := filepath.Join(root, "Cluster_1", "Master")
			control := newMaintenanceLuaControl(t, world)
			customPath := filepath.Join(world, customCommandsName)
			if original != "" {
				if err := os.WriteFile(customPath, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{"save/world.sav", "dst-admin/user-notes.txt"} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(world, path)), 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(world, path), []byte("keep user data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			control.delayLoads = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control.afterSend = func(script string) {
				if !strings.Contains(script, "local __update=") {
					return
				}
				file, err := os.OpenFile(customPath, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString("\nnew_user_setting = true\n")
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatalf("append user edit: %v %v", writeErr, closeErr)
				}
				cancel()
			}
			result, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}, control)
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrManagedFileChanged) || result.State != string(InstallStateMissing) {
				t.Fatalf("failed first installation recovery: %+v %v", result, err)
			}
			data, err := os.ReadFile(customPath)
			if err != nil || !strings.Contains(string(data), "new_user_setting = true") || !strings.Contains(string(data), original) || strings.Contains(string(data), "DST-ADMIN MANAGED BLOCK") {
				t.Fatalf("first installation lost user edits or restored loader: %s %v", data, err)
			}
			for _, path := range []string{"save/world.sav", "dst-admin/user-notes.txt"} {
				if data, err := os.ReadFile(filepath.Join(world, path)); err != nil || string(data) != "keep user data" {
					t.Fatalf("recovery modified user data %s: %s %v", path, data, err)
				}
			}
		})
	}
}
