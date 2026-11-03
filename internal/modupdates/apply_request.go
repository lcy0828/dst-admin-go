package modupdates

import (
	"context"
	"fmt"
	"slices"

	"dont/internal/jobs"
	"dont/internal/mods"
)

// Optional for old clients; new clients bind confirmation to a checked scope
// and exact candidate set. All clients still receive the execution-time check.
type ApplyRequest struct {
	ExpectedScopeRevision string   `json:"expectedScopeRevision,omitempty"`
	ModIDs                []string `json:"modIds,omitempty"`
}
type applyRequestKey struct{}

func (s *Service) SubmitApplyNowChecked(roomID string, request ApplyRequest) (jobs.Job, error) {
	if len(request.ExpectedScopeRevision) > 128 || len(request.ModIDs) > 512 {
		return jobs.Job{}, ErrInvalidInput
	}
	for _, id := range request.ModIDs {
		if !mods.ValidID(id) {
			return jobs.Job{}, ErrInvalidInput
		}
	}
	return s.submitChecked(roomID, DueApplyNow, request)
}
func checkApplyRequest(ctx context.Context, state State, checked mods.ActionResult) error {
	request, _ := ctx.Value(applyRequestKey{}).(ApplyRequest)
	if request.ExpectedScopeRevision != "" && (checked.CheckSummary == nil || checked.CheckSummary.Scope.Revision != request.ExpectedScopeRevision) {
		return fmt.Errorf("房间配置或影响范围已变化，请重新检查并确认")
	}
	if request.ModIDs != nil {
		ids := append([]string{}, checked.ModIDs...)
		if state.PendingUpdate != nil {
			ids = append(ids, state.PendingUpdate.ModIDs...)
		}
		if !slices.Equal(normalizedIDs(request.ModIDs), normalizedIDs(ids)) {
			return fmt.Errorf("可更新模组已变化，请重新检查并确认")
		}
	}
	return nil
}
