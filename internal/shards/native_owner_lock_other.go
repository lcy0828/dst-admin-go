//go:build !darwin && !linux

package shards

import (
	"fmt"
	"runtime"
)

type nativeRuntimeOwner struct{}

func acquireNativeRuntimeOwner(_, _ string) (*nativeRuntimeOwner, error) {
	return nil, fmt.Errorf("native tmux Runtime ownership is unsupported on %s", runtime.GOOS)
}

func (owner *nativeRuntimeOwner) close() error { return nil }
