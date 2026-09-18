package runtimeperformance

import (
	"path/filepath"
	"strings"

	"dont/internal/dstserver"
)

// ManagedLinuxLauncher builds the launch contract used by installation and inspection.
// The injector environment resolves the plugin directly; the upstream marker is a cache.
func ManagedLinuxLauncher(layout dstserver.Layout) (string, error) {
	modRelative, err := filepath.Rel(layout.WorkingDirectory, filepath.Join(layout.ContentRoot, "mods", "DontStarveLuaJIT2"))
	if err != nil {
		return "", err
	}
	rootRelative, err := filepath.Rel(layout.WorkingDirectory, layout.InstallRoot)
	if err != nil {
		return "", err
	}
	wrapper := "#!/bin/sh\n# DST Admin LuaJIT launcher v1\nset -eu\nbin_dir=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\n" +
		"mod_root=\"$bin_dir\"/" + shellLiteral(modRelative) + "\ninstall_root=\"$bin_dir\"/" + shellLiteral(rootRelative) + "\n" +
		"exec 9>\"$install_root/.dst-admin-luajit.lock\"\nflock -s -n 9 || exit 75\n[ ! -d \"$install_root/.dst-admin-luajit-transaction\" ] || exit 75\n" +
		"export LD_LIBRARY_PATH=\"$bin_dir/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\"\nexport DS_LUAJIT_INJECTOR=\"$mod_root/libInjector.so\"\nexport DS_LUAJIT_INJECTOR_DIR=\"$mod_root\"\nexport DS_LUAJIT_PLUGIN_DIR=\"$mod_root/plugins\"\nexport LD_PRELOAD=\"$bin_dir/lib64/libInjector.so\"\ncd \"$bin_dir\"\nexec \"$bin_dir/" + filepath.Base(layout.Executable) + "_1\" \"$@\"\n"

	return wrapper, nil
}

func shellLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
