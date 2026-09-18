//go:build linux || darwin

package tempfiles

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLock(file *os.File, exclusive bool) error {
	flags := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		flags = unix.LOCK_EX | unix.LOCK_NB
	}
	err := unix.Flock(int(file.Fd()), flags)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errBusy
	}
	return err
}
