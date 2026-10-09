//go:build linux || darwin

package softwareupdate

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func SupervisorAvailable() bool { return true }

func ManagedChild() bool {
	pid, err := strconv.Atoi(os.Getenv("DST_ADMIN_UPDATE_PARENT_PID"))
	// A control-plane image may exec the launcher as container PID 1. The
	// actual parent must still match the PID supplied by that launcher.
	return err == nil && managedParent(pid, os.Getppid(), os.Getenv("DST_ADMIN_SUPERVISED"))
}

func managedParent(declared, actual int, supervised string) bool {
	return declared > 0 && declared == actual && supervised == "1"
}

func wakeSupervisor() error {
	if !ManagedChild() {
		return ErrUnsupported
	}
	if os.Getenv("DST_ADMIN_AGENT_LAUNCHER_URL") != "" {
		return wakeAgentLauncher()
	}
	return syscall.Kill(os.Getppid(), syscall.SIGUSR1)
}

func updateSignals() (<-chan os.Signal, func()) {
	channel := make(chan os.Signal, 1)
	signal.Notify(channel, syscall.SIGUSR1)
	return channel, func() { signal.Stop(channel) }
}

func lockSupervisor(root string) (func(), error) {
	path := root + "/supervisor.lock"
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("software launcher is already running: %w", err)
	}
	return func() { unix.Flock(fd, unix.LOCK_UN); unix.Close(fd) }, nil
}

func terminateProcess(process *os.Process) error { return process.Signal(syscall.SIGTERM) }

func syncDirectory(directory *os.File) error { return directory.Sync() }
