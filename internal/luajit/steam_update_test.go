package luajit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSteamUpdatePreservesLauncherAndNewBinary(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "steam-failure"}[failed], func(t *testing.T) {
			_, release, archive := fixtureStore(t)
			o, game := fixtureInstallation(t, true)
			if _, err := Install(context.Background(), o, release, archive); err != nil {
				t.Fatal(err)
			}
			wrapper, _ := os.ReadFile(game)
			save := filepath.Join(o.ServerPath, "saves", "user-save")
			if err := writeFile(save, []byte("do not delete"), 0600); err != nil {
				t.Fatal(err)
			}
			newBinary := append(fixtureELF(), []byte("new Steam build")...)
			downloadErr := errors.New("download interrupted")
			err := PreserveSteamLauncher(o, func() error {
				if err := writeFile(game, newBinary, 0755); err != nil {
					return err
				}
				if failed {
					return downloadErr
				}
				return nil
			})
			if (failed && !errors.Is(err, downloadErr)) || (!failed && err != nil) {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(game)
			original, _ := os.ReadFile(game + "_1")
			if !bytes.Equal(after, wrapper) || !bytes.Equal(original, newBinary) || !o.Inspect().CanEnable {
				t.Fatal("launcher did not use new game executable")
			}
			if data, err := os.ReadFile(save); err != nil || string(data) != "do not delete" {
				t.Fatal("save changed", err)
			}
			if _, err := os.Stat(filepath.Join(o.ServerPath, ".dst-admin-luajit-steam-update")); !os.IsNotExist(err) {
				t.Fatal("intent not cleaned", err)
			}
		})
	}
}

func TestSteamUpdateRecoversInterruptedMaintenance(t *testing.T) {
	_, release, archive := fixtureStore(t)
	o, game := fixtureInstallation(t, true)
	if _, err := Install(context.Background(), o, release, archive); err != nil {
		t.Fatal(err)
	}
	if err := PreserveSteamLauncher(o, func() error { return writeFile(game, []byte("partial download"), 0755) }); err == nil {
		t.Fatal("partial binary accepted")
	}
	// A retry must actually allow SteamCMD to repair the partial executable.
	newBinary := append(fixtureELF(), []byte("recovered")...)
	if err := PreserveSteamLauncher(o, func() error { return writeFile(game, newBinary, 0755) }); err != nil {
		t.Fatal(err)
	}
	if !o.Inspect().CanEnable {
		t.Fatal("retry did not restore LuaJIT")
	}
	data, _ := os.ReadFile(game + "_1")
	if !bytes.Equal(data, newBinary) {
		t.Fatal("old executable restored")
	}
}

func TestSteamUpdateDoesNotEnableLuaJITOnVanilla(t *testing.T) {
	o, game := fixtureInstallation(t, true)
	data := append(fixtureELF(), []byte("vanilla update")...)
	if err := PreserveSteamLauncher(o, func() error { return writeFile(game, data, 0755) }); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(game)
	if !bytes.Equal(after, data) {
		t.Fatal("vanilla executable changed")
	}
	if _, err := os.Stat(game + "_1"); !os.IsNotExist(err) {
		t.Fatal("unexpected LuaJIT wrapper")
	}
}
