//go:build windows

package softwareupdate

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/windows"
)

// Management binaries are not published for Windows. The Agent launcher uses
// authenticated loopback IPC and version directories, avoiding a running EXE.
func SupervisorAvailable() bool { return false }
func ManagedChild() bool {
	pid, err := strconv.Atoi(os.Getenv("DST_ADMIN_UPDATE_PARENT_PID"))
	return err == nil && pid > 0 && pid == os.Getppid() && os.Getenv("DST_ADMIN_SUPERVISED") == "1"
}
func wakeSupervisor() error {
	if !ManagedChild() {
		return ErrUnsupported
	}
	return wakeAgentLauncher()
}
func updateSignals() (<-chan os.Signal, func())  { return make(chan os.Signal), func() {} }
func terminateProcess(process *os.Process) error { return process.Kill() }

func lockSupervisor(root string) (func(), error) {
	name, err := windows.UTF16PtrFromString(root + "\\supervisor.lock")
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("Agent launcher is already running: %w", err)
	}
	return func() { windows.UnlockFileEx(handle, 0, 1, 0, &overlapped); windows.CloseHandle(handle) }, nil
}

// Windows does not support FlushFileBuffers on a directory opened by os.Open.
// Each regular file is flushed before the same-volume atomic rename.
func syncDirectory(directory *os.File) error { return nil }
