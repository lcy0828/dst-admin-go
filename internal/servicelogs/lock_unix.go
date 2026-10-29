//go:build !windows

package servicelogs

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockWriter(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
