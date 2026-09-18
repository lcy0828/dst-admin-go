//go:build !linux && !darwin && !windows

package tempfiles

import (
	"errors"
	"os"
)

func tryLock(*os.File, bool) error {
	return errors.New("temporary artifact locks require Linux, macOS or Windows")
}
