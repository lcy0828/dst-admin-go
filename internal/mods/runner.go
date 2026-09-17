package mods

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"dont/internal/operationprogress"
	"dont/shared"
)

type DownloadRunner interface {
	Download(context.Context, []string, bool, io.Writer) error
}

type SessionDownloadRunner interface {
	DownloadSession(context.Context, []string, bool, io.Writer) error
}

type steamCMDSession struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	lines   chan string
	done    chan struct{}
	waitErr error
}

type SteamCMDRunner struct {
	Executable     string
	DownloadRoot   string
	AppID          string
	ContentLogPath string

	callMu      sync.Mutex
	sessionMu   sync.Mutex
	session     *steamCMDSession
	idleTimer   *time.Timer
	idleTimeout time.Duration
	active      bool
}

const defaultSteamCMDSessionIdleTimeout = time.Minute

var (
	steamCMDDownloadSuccessPattern = regexp.MustCompile(`(?i)Success\. Downloaded item ([1-9][0-9]{0,19})`)
	steamCMDDownloadFailurePattern = regexp.MustCompile(`(?i)ERROR! Download item ([1-9][0-9]{0,19}) failed \(([^)]+)\)`)
)

func NewSteamCMDRunner(configuredPath, downloadRoot, appID string) *SteamCMDRunner {
	return &SteamCMDRunner{
		Executable: findSteamCMD(configuredPath), DownloadRoot: downloadRoot, AppID: appID,
		idleTimeout: defaultSteamCMDSessionIdleTimeout,
	}
}

func (r *SteamCMDRunner) Download(ctx context.Context, ids []string, validate bool, output io.Writer) error {
	if r.Executable == "" {
		return ErrSteamCMDUnavailable
	}
	ids = uniqueModIDs(ids)
	if len(ids) == 0 {
		return ErrInvalidModID
	}
	arguments := []string{"+force_install_dir", r.DownloadRoot, "+login", "anonymous"}
	for _, id := range ids {
		arguments = append(arguments, "+workshop_download_item", r.AppID, id)
		if validate {
			arguments = append(arguments, "validate")
		}
	}
	arguments = append(arguments, "+quit")
	command := exec.CommandContext(ctx, r.Executable, arguments...)
	if output == nil {
		output = io.Discard
	}
	tail := &boundedTailWriter{limit: 64 * 1024}
	writer := io.MultiWriter(output, tail)
	command.Stdout = writer
	command.Stderr = writer
	if err := command.Start(); err != nil {
		return fmt.Errorf("%w: %v", ErrSteamCMDDownload, err)
	}
	stopProgress := r.followDownloadProgress(ctx, command.Process.Pid, ids)
	runErr := command.Wait()
	stopProgress()
	if failure := steamCMDOutputFailure(tail.String()); failure != "" {
		return fmt.Errorf("%w: %s", ErrSteamCMDDownload, failure)
	}
	if runErr != nil {
		return fmt.Errorf("%w: %v", ErrSteamCMDDownload, runErr)
	}
	return nil
}

// DownloadSession keeps one authenticated SteamCMD process alive for ordinary
// downloads to the runner's fixed install root. Exact-version staging uses
// Download instead, so temporary roots never leak into this reusable session.
func (r *SteamCMDRunner) DownloadSession(ctx context.Context, ids []string, validate bool, output io.Writer) error {
	if r.Executable == "" {
		return ErrSteamCMDUnavailable
	}
	ids = uniqueModIDs(ids)
	if len(ids) == 0 {
		return ErrInvalidModID
	}
	if output == nil {
		output = io.Discard
	}

	r.callMu.Lock()
	defer r.callMu.Unlock()
	items := make([]shared.ModDownloadProgress, len(ids))
	for index, id := range ids {
		items[index] = shared.ModDownloadProgress{WorkshopID: id, Status: "queued"}
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModCache, Message: "正在连接 Steam", TotalItems: len(ids), Items: append([]shared.ModDownloadProgress(nil), items...)})

	r.sessionMu.Lock()
	if r.idleTimer != nil {
		r.idleTimer.Stop()
		r.idleTimer = nil
	}
	r.active = true
	session, fresh, err := r.ensureSessionLocked()
	r.sessionMu.Unlock()
	if err != nil {
		r.finishSessionCall(nil, false)
		return err
	}

	if fresh {
		if _, err := io.WriteString(session.stdin, "force_install_dir "+steamCMDQuote(r.DownloadRoot)+"\nlogin anonymous\n"); err != nil {
			r.finishSessionCall(session, true)
			return fmt.Errorf("%w: %v", ErrSteamCMDDownload, err)
		}
	}
	for index, id := range ids {
		var itemMu sync.Mutex
		items[index].Status = "downloading"
		itemCtx := operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
			itemMu.Lock()
			defer itemMu.Unlock()
			if update.CurrentBytes > 0 || update.TotalBytes > 0 {
				items[index].CurrentBytes, items[index].TotalBytes, items[index].BytesPerSecond = update.CurrentBytes, update.TotalBytes, update.BytesPerSecond
			}
			update.Items = append([]shared.ModDownloadProgress(nil), items...)
			update.WorkshopID, update.CurrentItem, update.TotalItems = id, index+1, len(ids)
			update.Percent = (index*100 + min(100, max(0, update.Percent))) / len(ids)
			update.Message = fmt.Sprintf("模组 %d/%d · Workshop %s · %s", index+1, len(ids), id, update.Message)
			operationprogress.Report(ctx, update)
		})
		operationprogress.Report(itemCtx, operationprogress.Update{Stage: operationprogress.StageModCache, Message: "开始下载"})
		// SteamCMD remains logged in. Follow each item before issuing its command
		// so the next mod never inherits the previous mod's byte total.
		stopProgress := r.followDownloadProgress(itemCtx, session.command.Process.Pid, []string{id})
		err := r.downloadSessionItem(itemCtx, session, id, validate, output)
		stopProgress()
		if err != nil {
			items[index].Status, items[index].Message, items[index].BytesPerSecond = "failed", err.Error(), 0
			operationprogress.Report(itemCtx, operationprogress.Update{Stage: operationprogress.StageModCache, Message: "下载失败"})
			r.finishSessionCall(session, true)
			return err
		}
		items[index].Status, items[index].BytesPerSecond = "succeeded", 0
		if items[index].TotalBytes > 0 {
			items[index].CurrentBytes = items[index].TotalBytes
		}
		operationprogress.Report(itemCtx, operationprogress.Update{Stage: operationprogress.StageModCache, Percent: 100, Message: "下载完成"})
	}
	r.finishSessionCall(session, false)
	return nil
}

func (r *SteamCMDRunner) downloadSessionItem(ctx context.Context, session *steamCMDSession, id string, validate bool, output io.Writer) error {
	command := "workshop_download_item " + r.AppID + " " + id
	if validate {
		command += " validate"
	}
	if _, err := io.WriteString(session.stdin, command+"\n"); err != nil {
		return fmt.Errorf("%w: %v", ErrSteamCMDDownload, err)
	}
	tail := &boundedTailWriter{limit: 64 * 1024}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrSteamCMDDownload, ctx.Err())
		case line, ok := <-session.lines:
			if !ok {
				failure := steamCMDOutputFailure(tail.String())
				if failure != "" {
					return fmt.Errorf("%w: %s", ErrSteamCMDDownload, failure)
				}
				select {
				case <-session.done:
					if session.waitErr != nil {
						return fmt.Errorf("%w: %v", ErrSteamCMDDownload, session.waitErr)
					}
				case <-ctx.Done():
					return fmt.Errorf("%w: %v", ErrSteamCMDDownload, ctx.Err())
				}
				return fmt.Errorf("%w: SteamCMD exited before all Workshop items completed", ErrSteamCMDDownload)
			}
			line += "\n"
			_, _ = io.WriteString(output, line)
			_, _ = io.WriteString(tail, line)
			if match := steamCMDDownloadFailurePattern.FindStringSubmatch(line); len(match) == 3 && match[1] == id {
				return fmt.Errorf("%w: %s", ErrSteamCMDDownload, strings.TrimSpace(match[2]))
			}
			if match := steamCMDDownloadSuccessPattern.FindStringSubmatch(line); len(match) == 2 && match[1] == id {
				return nil
			}
		case <-session.done:
			failure := steamCMDOutputFailure(tail.String())
			if failure != "" {
				return fmt.Errorf("%w: %s", ErrSteamCMDDownload, failure)
			}
			if session.waitErr != nil {
				return fmt.Errorf("%w: %v", ErrSteamCMDDownload, session.waitErr)
			}
			return fmt.Errorf("%w: SteamCMD exited before all Workshop items completed", ErrSteamCMDDownload)
		}
	}
}

func (r *SteamCMDRunner) ensureSessionLocked() (*steamCMDSession, bool, error) {
	if r.session != nil {
		select {
		case <-r.session.done:
			r.session = nil
		default:
			return r.session, false, nil
		}
	}
	command := exec.Command(r.Executable)
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrSteamCMDDownload, err)
	}
	command.Stdout, command.Stderr = pipeWriter, pipeWriter
	stdin, err := command.StdinPipe()
	if err != nil {
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
		return nil, false, fmt.Errorf("%w: %v", ErrSteamCMDDownload, err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
		return nil, false, fmt.Errorf("%w: %v", ErrSteamCMDDownload, err)
	}
	_ = pipeWriter.Close()
	session := &steamCMDSession{command: command, stdin: stdin, lines: make(chan string, 128), done: make(chan struct{})}
	r.session = session
	go func() {
		scanner := bufio.NewScanner(pipeReader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			session.lines <- scanner.Text()
		}
		_ = pipeReader.Close()
		close(session.lines)
	}()
	go func() {
		session.waitErr = command.Wait()
		close(session.done)
	}()
	return session, true, nil
}

func (r *SteamCMDRunner) finishSessionCall(session *steamCMDSession, discard bool) {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	r.active = false
	if discard && session != nil && r.session == session {
		r.closeSessionLocked(false)
		return
	}
	if session == nil || r.session != session {
		return
	}
	idleTimeout := r.idleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultSteamCMDSessionIdleTimeout
	}
	r.idleTimer = time.AfterFunc(idleTimeout, func() {
		r.sessionMu.Lock()
		defer r.sessionMu.Unlock()
		if !r.active && r.session == session {
			r.closeSessionLocked(true)
		}
	})
}

func (r *SteamCMDRunner) closeSessionLocked(graceful bool) {
	if r.idleTimer != nil {
		r.idleTimer.Stop()
		r.idleTimer = nil
	}
	session := r.session
	r.session = nil
	if session == nil {
		return
	}
	if graceful {
		_, _ = io.WriteString(session.stdin, "quit\n")
		select {
		case <-session.done:
			_ = session.stdin.Close()
			return
		case <-time.After(2 * time.Second):
		}
	}
	_ = session.stdin.Close()
	if session.command.Process != nil {
		_ = session.command.Process.Kill()
	}
}

func (r *SteamCMDRunner) Close() error {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	r.active = false
	r.closeSessionLocked(true)
	return nil
}

func steamCMDQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

type boundedTailWriter struct {
	buffer bytes.Buffer
	limit  int
}

func (w *boundedTailWriter) Write(value []byte) (int, error) {
	if w.limit <= 0 {
		return len(value), nil
	}
	if len(value) >= w.limit {
		w.buffer.Reset()
		_, _ = w.buffer.Write(value[len(value)-w.limit:])
		return len(value), nil
	}
	if excess := w.buffer.Len() + len(value) - w.limit; excess > 0 {
		current := append([]byte(nil), w.buffer.Bytes()[excess:]...)
		w.buffer.Reset()
		_, _ = w.buffer.Write(current)
	}
	_, _ = w.buffer.Write(value)
	return len(value), nil
}

func (w *boundedTailWriter) String() string { return w.buffer.String() }

func steamCMDOutputFailed(output string) bool {
	return steamCMDOutputFailure(output) != ""
}

func steamCMDOutputFailure(output string) string {
	value := strings.ToLower(output)
	if strings.Contains(value, "error! download item") && strings.Contains(value, "failed") {
		if start := strings.LastIndex(value, "failed ("); start >= 0 {
			start += len("failed (")
			if end := strings.IndexByte(output[start:], ')'); end >= 0 {
				return strings.TrimSpace(output[start : start+end])
			}
		}
		return "SteamCMD reported a Workshop download failure"
	}
	if strings.Contains(value, "update canceled:") && strings.Contains(value, "failure") {
		if start := strings.LastIndex(value, "("); start >= 0 {
			start++
			if end := strings.IndexByte(output[start:], ')'); end >= 0 {
				return strings.TrimSpace(output[start : start+end])
			}
		}
		return "SteamCMD update was canceled by a storage failure"
	}
	return ""
}

func findSteamCMD(configured string) string {
	configured = filepath.Clean(strings.TrimSpace(configured))
	if configured != "" && configured != "." {
		if executableFile(configured) {
			return configured
		}
		for _, name := range []string{"steamcmd.sh", "steamcmd", "steamcmd.exe"} {
			candidate := filepath.Join(configured, name)
			if executableFile(candidate) {
				return candidate
			}
		}
	}
	for _, name := range []string{"steamcmd", "steamcmd.sh", "steamcmd.exe"} {
		if value, err := exec.LookPath(name); err == nil {
			return value
		}
	}
	return ""
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0
}

var _ DownloadRunner = (*SteamCMDRunner)(nil)
var _ SessionDownloadRunner = (*SteamCMDRunner)(nil)
