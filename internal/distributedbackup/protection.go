package distributedbackup

import (
	"context"

	"dont/internal/roomops"
)

// VerifyProtection validates the original artifacts before a release retry.
// Configuration-only backups are valid for rooms that have never been started.
func (c *Coordinator) VerifyProtection(ctx context.Context, id, roomID, sourceJobID string) error {
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return err
	}
	defer release()
	value, err := c.store.GetSet(id)
	if err != nil {
		return err
	}
	if value.Kind != "protection" || value.RoomID != roomID || value.SourceJobID != sourceJobID {
		return ErrIntegrity
	}
	if _, err := c.setDirectory(id); err != nil {
		return err
	}
	_, parts, revision, _, err := c.plan(ctx, roomID)
	if err != nil {
		return err
	}
	return c.verifySet(ctx, value, parts, revision, false)
}
