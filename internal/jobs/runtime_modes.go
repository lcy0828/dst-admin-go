package jobs

import (
	"encoding/json"
	"errors"
	"reflect"

	"dont/shared"
)

// RuntimeModeSelection is the immutable per-world launch plan for this job.
// Keeping it on the job gives it the existing job retention and recovery rules.
type RuntimeModeSelection struct {
	Error              *Error                        `json:"error,omitempty"`
	RoomID             string                        `json:"roomId"`
	WorldID            string                        `json:"worldId"`
	WorldName          string                        `json:"worldName"`
	Action             string                        `json:"action"`
	RequestedMode      shared.RuntimePerformanceMode `json:"requestedMode,omitempty"`
	RequestedVersion   string                        `json:"requestedVersion,omitempty"`
	Mode               shared.RuntimePerformanceMode `json:"mode"`
	Source             string                        `json:"source"`
	TargetID           string                        `json:"targetId,omitempty"`
	InstallationID     string                        `json:"installationId,omitempty"`
	TopologyRevision   string                        `json:"topologyRevision,omitempty"`
	WasRunning         bool                          `json:"wasRunning"`
	PreferenceRevision string                        `json:"preferenceRevision,omitempty"`
}

// PlacementUnresolved is only true when planning failed before a target was
// captured. A retry may fill this binding, but must never replace a known one.
func (s RuntimeModeSelection) PlacementUnresolved() bool {
	return s.Error != nil && s.TargetID == "" && s.InstallationID == "" && s.TopologyRevision == ""
}

func (s *Store) RuntimeModePlan(jobID, roomID string) ([]RuntimeModeSelection, error) {
	job, err := s.Get(jobID)
	if err != nil {
		return nil, err
	}
	var result []RuntimeModeSelection
	for _, value := range job.RuntimeModes {
		if value.RoomID == roomID {
			result = append(result, value)
		}
	}
	return result, nil
}

func (s *Store) SaveRuntimeModePlan(jobID, roomID string, plan []RuntimeModeSelection) error {
	if len(plan) == 0 {
		return errors.New("empty runtime mode plan")
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	var record jobRecord
	if err := tx.Table(s.jobsTable).Where("id = ?", jobID).First(&record).Error; err != nil {
		return err
	}
	var all []RuntimeModeSelection
	if record.RuntimeModes != "" {
		if err := json.Unmarshal([]byte(record.RuntimeModes), &all); err != nil {
			return err
		}
	}
	var existing []RuntimeModeSelection
	for _, value := range all {
		if value.RoomID == roomID {
			existing = append(existing, value)
		}
	}
	if len(existing) > 0 {
		if len(existing) != len(plan) {
			return errors.New("job runtime mode plan changed")
		}
		for i, original := range existing {
			candidate := plan[i]
			if original.PlacementUnresolved() {
				original.TargetID, original.InstallationID, original.TopologyRevision = candidate.TargetID, candidate.InstallationID, candidate.TopologyRevision
			}
			if original.Error != nil && original.Source == "unknown" {
				original.Mode, original.Source = candidate.Mode, candidate.Source
				original.PreferenceRevision, original.WasRunning = candidate.PreferenceRevision, candidate.WasRunning
			}
			if original.Action == "start" {
				// A replay observes whether start is now a no-op. Its resolved mode
				// remains frozen even if the process stopped or started meanwhile.
				original.WasRunning = candidate.WasRunning
			}
			original.Error, candidate.Error = nil, nil
			if !reflect.DeepEqual(original, candidate) {
				return errors.New("job runtime mode plan changed")
			}
		}
		kept := all[:0]
		for _, value := range all {
			if value.RoomID != roomID {
				kept = append(kept, value)
			}
		}
		all = kept
	}
	for _, value := range plan {
		if value.RoomID != roomID || value.WorldID == "" || value.Mode == "" {
			return errors.New("invalid runtime mode plan")
		}
		if _, ok := shared.NormalizeRuntimePerformanceMode(value.Mode); !ok {
			return errors.New("invalid runtime mode")
		}
	}
	encoded, err := json.Marshal(append(all, plan...))
	if err != nil {
		return err
	}
	if err := tx.Table(s.jobsTable).Where("id = ?", jobID).UpdateColumn("runtime_modes", string(encoded)).Error; err != nil {
		return err
	}
	return tx.Commit().Error
}
