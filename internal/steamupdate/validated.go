// Package steamupdate implements the bounded, validated SteamCMD write shared
// by the in-process Runtime and Agents. Callers own installation/process locks.
package steamupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type Runner interface {
	Run(context.Context, string, []string, io.Writer) error
}

var stateFlagsPattern = regexp.MustCompile(`(?mi)^\s*"StateFlags"\s+"([0-9]+)"\s*$`)

// IntegrityError reads only the first existing manifest, using the same
// precedence as version observation. Older manifests without StateFlags remain
// compatible; an explicit non-installed state must never count as verification.
func IntegrityError(paths ...string) error {
	for _, path := range paths {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("读取 Steam 安装状态失败: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		_ = f.Close()
		if readErr != nil || len(data) > 1<<20 {
			return errors.New("Steam 安装清单无法读取或超过大小限制")
		}
		match := stateFlagsPattern.FindSubmatch(data)
		if len(match) == 0 {
			if strings.Contains(strings.ToLower(string(data)), `"stateflags"`) {
				return errors.New("Steam 安装状态字段无效，请执行更新并校验")
			}
			return nil
		}
		flags, err := strconv.ParseUint(string(match[1]), 10, 64)
		if err != nil || flags != 4 {
			return fmt.Errorf("Steam 安装尚未完整就绪 (StateFlags=%s)，请执行更新并校验", match[1])
		}
		return nil
	}
	return nil
}

// RunValidated retries at most once, only after SteamCMD has returned an exact
// terminal corrupt-files failure for this App. A timeout, disconnect, login
// failure, or ambiguous receipt never authorizes a resend here.
func RunValidated(ctx context.Context, runner Runner, executable, root, appID string, manifests []string, output io.Writer) error {
	arguments := []string{"+force_install_dir", root, "+login", "anonymous", "+app_update", appID, "validate", "+quit"}
	failure := regexp.MustCompile(`(?mi)^\s*Error! App '` + regexp.QuoteMeta(appID) + `' state is (0x[0-9a-f]+) after update job\.\s*$`)
	for attempt := 1; attempt <= 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(output, "[DST Admin] SteamCMD app=%s validate=true attempt=%d/2\n", appID, attempt)
		capture := &tailWriter{out: output}
		err := runner.Run(ctx, executable, arguments, capture)
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		match := failure.FindStringSubmatch(capture.tail)
		if len(match) > 0 {
			state, _ := strconv.ParseUint(match[1][2:], 16, 64)
			err = errors.Join(err, fmt.Errorf("SteamCMD App %s 更新失败，状态 %s；校验未完成", appID, match[1]))
			if attempt == 1 && state&0x80 != 0 && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				_, _ = io.WriteString(output, "[DST Admin] SteamCMD 已退出并报告文件损坏，执行一次受限校验修复；保留安装清单与存档。\n")
				continue
			}
		}
		if err != nil {
			return err
		}
		return IntegrityError(manifests...)
	}
	panic("unreachable")
}

type tailWriter struct {
	mu   sync.Mutex
	out  io.Writer
	tail string
}

func (w *tailWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail += string(data)
	if len(w.tail) > 64<<10 {
		w.tail = w.tail[len(w.tail)-(64<<10):]
	}
	return w.out.Write(data)
}
