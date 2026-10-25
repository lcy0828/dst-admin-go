package luajit

import (
	"context"
	"debug/elf"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Check the shipped binaries on every CI platform. An Ubuntu-built dependency
// or an executable stack must not silently break the Debian 12 Runtime/Agent.
func TestBundledLinuxABIBaseline(t *testing.T) {
	packages, err := bundledPackages()
	if err != nil || len(packages) == 0 {
		t.Fatalf("bundled packages: %v", err)
	}
	for _, pkg := range packages {
		t.Run(pkg.File, func(t *testing.T) {
			data, err := bundled.ReadFile("packages/" + pkg.File)
			if err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "package.zip")
			if err := os.WriteFile(archive, data, 0600); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if _, err := unpack(context.Background(), archive, root); err != nil {
				t.Fatal(err)
			}
			count := 0
			err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() || !strings.Contains(entry.Name(), ".so") {
					return err
				}
				file, err := elf.Open(path)
				if err != nil {
					return err
				}
				defer file.Close()
				count++
				name, _ := filepath.Rel(root, path)
				if file.Class != elf.ELFCLASS64 || file.Machine != elf.EM_X86_64 || file.Type != elf.ET_DYN {
					t.Errorf("%s is not a Linux x64 shared library", name)
				}
				for _, program := range file.Progs {
					if program.Type == elf.PT_GNU_STACK && program.Flags&elf.PF_X != 0 {
						t.Errorf("%s requires an executable stack", name)
					}
				}
				symbols, err := file.ImportedSymbols()
				if err != nil {
					return err
				}
				for _, symbol := range symbols {
					for prefix, maximum := range map[string]string{
						"GLIBC_": "2.36", "GLIBCXX_": "3.4.30", "CXXABI_1.": "3.13",
					} {
						if strings.HasPrefix(symbol.Version, prefix) && !abiAtMost(strings.TrimPrefix(symbol.Version, prefix), maximum) {
							t.Errorf("%s requires %s (%s), newer than Debian 12", name, symbol.Version, symbol.Name)
						}
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if count == 0 {
				t.Fatal("bundle contains no shared libraries")
			}
		})
	}
}

func abiAtMost(version, maximum string) bool {
	parts, limit := strings.Split(version, "."), strings.Split(maximum, ".")
	for index := 0; index < len(parts) || index < len(limit); index++ {
		left, right := 0, 0
		var err error
		if index < len(parts) {
			left, err = strconv.Atoi(parts[index])
			if err != nil {
				return false
			}
		}
		if index < len(limit) {
			right, _ = strconv.Atoi(limit[index])
		}
		if left != right {
			return left < right
		}
	}
	return true
}
