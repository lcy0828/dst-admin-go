// Package maintenance supplies live-player checks for unattended operations.
package maintenance

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrPlayersOnline       = errors.New("受影响房间仍有玩家在线，等待下次检查")
	ErrPresenceUnavailable = errors.New("无法确认受影响房间无人，已跳过自动维护")
)

type OnlineCounter interface {
	OnlinePlayers(context.Context, string) (int, error)
}

func RequireEmpty(ctx context.Context, counter OnlineCounter, roomIDs []string) error {
	if counter == nil || len(roomIDs) == 0 {
		return ErrPresenceUnavailable
	}
	for _, roomID := range roomIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := counter.OnlinePlayers(ctx, roomID)
		if err != nil {
			return fmt.Errorf("%w (%s): %w", ErrPresenceUnavailable, roomID, err)
		}
		if count < 0 {
			return ErrPresenceUnavailable
		}
		if count > 0 {
			return fmt.Errorf("%w (%s)", ErrPlayersOnline, roomID)
		}
	}
	return nil
}

type checkKey struct{}

// WithCheck lets a restart recheck after notifications and lock acquisition.
func WithCheck(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, checkKey{}, check)
}

func Check(ctx context.Context) error {
	if check, ok := ctx.Value(checkKey{}).(func(context.Context) error); ok {
		return check(ctx)
	}
	return nil
}
