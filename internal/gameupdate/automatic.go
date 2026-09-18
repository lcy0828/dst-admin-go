package gameupdate

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// UpdateRoomWhenEmpty includes every room sharing this room's installations,
// including installations on different Agents. Unrelated installations are excluded.
func (c *ReleaseCoordinator) UpdateRoomWhenEmpty(ctx context.Context, roomID, jobID string) (bool, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return false, ErrReleaseInvalid
	}
	plan, err := c.planner.Preview(ctx, ReleasePreviewRequest{
		roomID: roomID, Policy: ReleasePolicyInput{RequireEmpty: true},
	})
	if err != nil {
		return false, err
	}
	if !plan.Ready {
		messages := make([]string, 0, len(plan.Blockers))
		for _, blocker := range plan.Blockers {
			messages = append(messages, blocker.Message)
		}
		return false, fmt.Errorf("%w: %s", ErrReleasePreviewBlocked, strings.Join(messages, "; "))
	}
	if err := c.store.requireNoPendingRelease(plan); err != nil {
		return false, err
	}
	if !plan.UpdateRequired {
		return false, nil
	}
	value, err := c.Publish(ctx, ReleasePublishRequest{ID: uuid.NewString(), SourceJobID: jobID, Plan: plan})
	if err != nil {
		return false, err
	}
	if value.Stage != ReleaseStageSucceeded {
		return false, ErrReleaseRecoveryNeeded
	}
	return true, nil
}
