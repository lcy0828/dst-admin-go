package gameupdate

import (
	"context"
	"encoding/json"

	"dont/internal/installationmaintenance"
)

func releaseResources(plan ReleasePlan) []installationmaintenance.Resource {
	values := make([]installationmaintenance.Resource, 0, len(plan.Installations))
	for _, target := range plan.Installations {
		values = append(values, installationmaintenance.Resource{TargetID: target.TargetID, InstallationID: target.InstallationID, Directory: target.ServerPath})
	}
	return values
}

// CheckMaintenance also protects interrupted writes after Controller restart.
// Failed pre-write attempts have already restored their worlds and do not hold
// an installation; recovery_required attempts retain ownership until retry.
func (s *ReleaseStore) CheckMaintenance(ctx context.Context, resources []installationmaintenance.Resource, owner string) error {
	var records []releaseRecord
	if err := s.db.Table(s.releaseTable).Where("stage NOT IN (?)", []string{string(ReleaseStageSucceeded), string(ReleaseStageFailed), string(ReleaseStageAbandoned)}).Find(&records).Error; err != nil {
		return err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.ID == owner {
			continue
		}
		var plan ReleasePlan
		if err := json.Unmarshal([]byte(record.PlanJSON), &plan); err != nil {
			return err
		}
		for _, held := range releaseResources(plan) {
			for _, resource := range resources {
				if installationmaintenance.Overlaps(held, resource) {
					return &installationmaintenance.BusyError{Owner: record.ID}
				}
			}
		}
	}
	return nil
}
func (c *ReleaseCoordinator) ConfigureMaintenance(gate *installationmaintenance.Gate) {
	c.maintenance = gate
}
