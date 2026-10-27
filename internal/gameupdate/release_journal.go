package gameupdate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"dont/internal/installationmaintenance"
	"dont/internal/runtimedriver"
	"dont/shared"
)

// Native update evidence survives a Controller restart. Agent updates already
// use the Agent's persistent, fenced Runtime operation journal.
func (s *Service) UpdateReleaseInstallationOperation(ctx context.Context, operation runtimedriver.Operation, expected string, cleanCache bool) (shared.RuntimeGameVersionResult, error) {
	ctx, release, err := s.maintenance.Acquire(ctx, []installationmaintenance.Resource{{TargetID: "local", InstallationID: "default", Directory: installRoot(s.config.ServerPath)}}, "")
	if err != nil {
		return shared.RuntimeGameVersionResult{}, errors.Join(runtimedriver.ErrOperationNotDispatched, err)
	}
	defer release()
	if evidence, err := s.ObserveReleaseUpdate(ctx, operation.ID); err == nil {
		if !evidence.Completed {
			return shared.RuntimeGameVersionResult{}, errors.New("上次本机游戏更新结果待确认")
		}
		current, err := s.ObserveReleaseInstallation(ctx)
		if evidence.Outcome != shared.RuntimeOutcomeConfirmed {
			err = errors.Join(err, errors.New(evidence.Message))
		}
		return current, err
	}
	evidence := shared.RuntimeOperationEvidence{OperationID: operation.ID, Action: string(shared.RuntimeActionGameVersionUpdate), ObservedAt: s.now().UTC()}
	if err := s.writeReleaseEvidence(evidence); err != nil {
		return shared.RuntimeGameVersionResult{}, errors.Join(runtimedriver.ErrOperationNotDispatched, err)
	}
	result, err := s.UpdateReleaseInstallation(ctx, expected, cleanCache)
	evidence.Completed, evidence.ObservedAt, evidence.Outcome = true, s.now().UTC(), shared.RuntimeOutcomeConfirmed
	if err != nil {
		evidence.Outcome, evidence.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	if saveErr := s.writeReleaseEvidence(evidence); saveErr != nil {
		return shared.RuntimeGameVersionResult{}, errors.Join(err, saveErr)
	}
	return result, err
}
func (s *Service) releaseEvidencePath() string {
	return filepath.Join(installRoot(s.config.ServerPath), ".dst-admin-game-update.json")
}
func (s *Service) writeReleaseEvidence(evidence shared.RuntimeOperationEvidence) error {
	data, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(installRoot(s.config.ServerPath), ".dst-admin-update-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), s.releaseEvidencePath())
}
func (s *Service) ObserveReleaseUpdate(ctx context.Context, id string) (shared.RuntimeOperationEvidence, error) {
	if err := ctx.Err(); err != nil {
		return shared.RuntimeOperationEvidence{}, err
	}
	data, err := os.ReadFile(s.releaseEvidencePath())
	if err != nil {
		return shared.RuntimeOperationEvidence{}, err
	}
	var evidence shared.RuntimeOperationEvidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		return evidence, err
	}
	if evidence.OperationID != id {
		return evidence, errors.New("未找到本次游戏更新的操作回执")
	}
	return evidence, nil
}
