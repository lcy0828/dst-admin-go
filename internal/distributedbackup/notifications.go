package distributedbackup

import (
	"context"
	"errors"
	"time"

	"dont/internal/operationlease"
)

type BackupNotifier interface {
	BeforeOperation(context.Context, string, string, string, string) error
}

func (c *Coordinator) ConfigureNotifier(notifier BackupNotifier) {
	c.notifier = notifier
}

// Countdowns can exceed the ordinary lease TTL. Renew only while this operation
// is waiting, cancel the countdown on lease loss, and join before using the lease.
func (c *Coordinator) runWithLeaseRenewal(ctx context.Context, lease *operationlease.Lease, interval time.Duration, run func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.renewLease(ctx, lease); err != nil {
		return err
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		lease operationlease.Lease
		err   error
	}
	finished := make(chan outcome, 1)
	current := *lease
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				finished <- outcome{lease: current}
				return
			case <-ticker.C:
				next, err := c.leases.Renew(waitCtx, current, leaseTTL)
				if err != nil {
					if waitCtx.Err() != nil {
						finished <- outcome{lease: current}
						return
					}
					cancel()
					finished <- outcome{lease: current, err: err}
					return
				}
				current = next
			}
		}
	}()
	err := run(waitCtx)
	cancel()
	result := <-finished
	*lease = result.lease
	return errors.Join(err, result.err, ctx.Err())
}
