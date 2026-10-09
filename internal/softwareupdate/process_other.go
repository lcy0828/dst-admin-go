//go:build !linux && !darwin && !windows

package softwareupdate

import "os"

func SupervisorAvailable() bool                  { return false }
func ManagedChild() bool                         { return false }
func wakeSupervisor() error                      { return ErrUnsupported }
func updateSignals() (<-chan os.Signal, func())  { return make(chan os.Signal), func() {} }
func lockSupervisor(string) (func(), error)      { return nil, ErrUnsupported }
func terminateProcess(process *os.Process) error { return process.Kill() }

func syncDirectory(directory *os.File) error { return directory.Sync() }
