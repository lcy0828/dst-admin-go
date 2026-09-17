package luajit

import (
	"archive/zip"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const MaxPackageBytes int64 = 256 << 20
const maxExpandedBytes uint64 = 1 << 30

var ErrInvalidPackage = errors.New("LuaJIT 安装包无效")
var versionLine = regexp.MustCompile(`(?m)^\s*version\s*=\s*["']([0-9]+\.[0-9]+\.[0-9]+)["']`)
var releaseVersion = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)
var packageID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var requiredFiles = []string{"modinfo.lua", "modmain.lua", "inject_server_only_mod.lua", "plugins/init.lua", "libInjector.so", "bin64/linux/lib64/libInjector.so", "plugins/plugin_core_vm/plugin_core_vm.so", "deps/liblua51DS.so"}

func unpack(ctx context.Context, archive, destination string) (string, error) {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidPackage, err)
	}
	defer z.Close()
	if len(z.File) == 0 || len(z.File) > 20000 {
		return "", ErrInvalidPackage
	}
	files := map[string]*zip.File{}
	var total uint64
	for _, f := range z.File {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		name := strings.TrimPrefix(f.Name, "Mod/")
		if f.Name == "Mod/" {
			continue
		}
		clean := path.Clean(name)
		if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") || (clean != strings.TrimSuffix(name, "/")) {
			return "", ErrInvalidPackage
		}
		if f.Mode()&os.ModeSymlink != 0 || (!f.FileInfo().IsDir() && !f.Mode().IsRegular()) {
			return "", fmt.Errorf("%w: 不允许链接或特殊文件", ErrInvalidPackage)
		}
		if _, exists := files[clean]; exists {
			return "", ErrInvalidPackage
		}
		files[clean] = f
		if f.UncompressedSize64 > maxExpandedBytes-total {
			return "", fmt.Errorf("%w: 解压大小超限", ErrInvalidPackage)
		}
		total += f.UncompressedSize64
		if f.FileInfo().IsDir() {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return "", err
		}
		r, err := f.Open()
		if err != nil {
			return "", err
		}
		mode := os.FileMode(0644)
		if f.Mode()&0111 != 0 || strings.HasSuffix(clean, ".so") {
			mode = 0755
		}
		w, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			r.Close()
			return "", err
		}
		n, copyErr := io.Copy(w, io.LimitReader(r, int64(f.UncompressedSize64)+1))
		closeErr := w.Close()
		r.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if uint64(n) != f.UncompressedSize64 {
			return "", ErrInvalidPackage
		}
	}
	for _, name := range requiredFiles {
		f := files[name]
		if f == nil || f.FileInfo().IsDir() {
			return "", fmt.Errorf("%w: 缺少 %s", ErrInvalidPackage, name)
		}
	}
	for _, name := range []string{"libInjector.so", "bin64/linux/lib64/libInjector.so", "plugins/plugin_core_vm/plugin_core_vm.so", "deps/liblua51DS.so"} {
		f, err := elf.Open(filepath.Join(destination, name))
		if err != nil {
			return "", fmt.Errorf("%w: %s 不是 ELF 动态库", ErrInvalidPackage, name)
		}
		valid := f.Class == elf.ELFCLASS64 && f.Machine == elf.EM_X86_64 && f.Type == elf.ET_DYN
		f.Close()
		if !valid {
			return "", fmt.Errorf("%w: 仅支持 Linux x64 安装包", ErrInvalidPackage)
		}
	}
	info, err := os.ReadFile(filepath.Join(destination, "modinfo.lua"))
	if err != nil {
		return "", err
	}
	match := versionLine.FindSubmatch(info)
	if len(match) != 2 {
		return "", fmt.Errorf("%w: 缺少版本号", ErrInvalidPackage)
	}
	return string(match[1]), nil
}
