package luajit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CheckDependencies runs on the target node before any live installation files
// are replaced. Catalog reads and normal world starts do not run this check.
func CheckDependencies(ctx context.Context, packageRoot, gameBin string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	env := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "LD_") && !strings.HasPrefix(value, "LC_ALL=") {
			env = append(env, value)
		}
	}
	env = append(env, "LC_ALL=C", "LD_LIBRARY_PATH="+strings.Join([]string{packageRoot, filepath.Join(packageRoot, "deps"), filepath.Join(gameBin, "lib64")}, ":"))
	return filepath.WalkDir(packageRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.Contains(d.Name(), ".so") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		var magic [4]byte
		n, _ := f.Read(magic[:])
		f.Close()
		if n != 4 || string(magic[:]) != "\x7fELF" {
			return nil
		}
		command := exec.CommandContext(ctx, "ldd", path)
		command.Env = env
		output, runErr := command.CombinedOutput()
		if err := dependencyResult(string(output), runErr); err != nil {
			return fmt.Errorf("LuaJIT 安装包与当前机器的运行库不兼容（%s）：%w；可选择适用于当前系统的兼容构建", d.Name(), err)
		}
		return nil
	})
}
func dependencyResult(output string, runErr error) error {
	issues := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "not found") || strings.Contains(line, "error while loading") {
			issues = append(issues, line)
			if len(issues) == 4 {
				break
			}
		}
	}
	if len(issues) > 0 {
		return errors.New(strings.Join(issues, "; "))
	}
	if runErr != nil {
		return fmt.Errorf("运行库检查失败: %w", runErr)
	}
	return nil
}
