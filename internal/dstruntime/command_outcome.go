package dstruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Dispatch already succeeded. Any later error leaves execution unknown and
// must never be interpreted as proof that it is safe to replay a mutation.
func unconfirmedCommand(request CommandRequest, cause, lastRead error) error {
	return fmt.Errorf("%w: %w: command %s may have executed; it will not be retried automatically: %w", ErrRuntimeCommandUnconfirmed, ErrRuntimeResultAbsent, request.RequestID, errors.Join(cause, lastRead))
}

// Older health documents lack a module identity. Their UUID/action/session/
// shard/time correlation remains compatible; new collectors advertise the
// command producer explicitly (it differs from the telemetry producer).
func commandInstanceMatches(health Health, receipt CommandReceipt) bool {
	instance := health.Modules["commands"].ProducerInstanceID
	return instance == "" || receipt.ProducerInstanceID == instance
}

// Wait only while this request is interested, without a goroutine per queued
// command. There is no ticker when a world has no queued work.
func lockRuntimeCommand(ctx context.Context, lock *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lock.TryLock() {
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if lock.TryLock() {
				if err := ctx.Err(); err != nil {
					lock.Unlock()
					return err
				}
				return nil
			}
		}
	}
}
