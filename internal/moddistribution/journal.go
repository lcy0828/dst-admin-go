package moddistribution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (m *Manager) Recover(ctx context.Context) (results []RecoveryResult, returnedErr error) {
	m.mu.Lock()
	defer func() {
		for _, result := range results {
			if result.Action == "blocked" && result.Error != "" {
				m.recoveryIssues[result.OperationID] = result.Error
			} else {
				delete(m.recoveryIssues, result.OperationID)
			}
		}
		m.mu.Unlock()
	}()
	items, err := os.ReadDir(filepath.Join(m.stateRoot, "journals"))
	if err != nil {
		return nil, err
	}
	results = make([]RecoveryResult, 0, len(items))
	var combined error
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return results, errors.Join(combined, err)
		}
		operationID := strings.TrimSuffix(item.Name(), ".json")
		journal, readErr := m.readJournal(operationID)
		if readErr != nil {
			results = append(results, RecoveryResult{OperationID: operationID, Action: "blocked", Error: readErr.Error()})
			combined = errors.Join(combined, readErr)
			continue
		}
		action := "rolled_back"
		var recoveryErr error
		if journal.Phase == PhaseCommitted || journal.Phase == PhaseCompleting && m.statesCommitted(journal.Plan) {
			action = "completed"
			recoveryErr = m.cleanupJournalArtifacts(journal)
			if recoveryErr == nil {
				recoveryErr = os.Remove(m.journalPath(operationID))
			}
		} else {
			recoveryErr = m.rollbackJournal(ctx, &journal)
		}
		result := RecoveryResult{OperationID: operationID, Action: action}
		if recoveryErr != nil {
			result.Action, result.Error = "blocked", recoveryErr.Error()
			combined = errors.Join(combined, recoveryErr)
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].OperationID < results[j].OperationID })
	return results, combined
}

func (m *Manager) readJournal(operationID string) (Journal, error) {
	if !operationIDPattern.MatchString(operationID) {
		return Journal{}, ErrInvalidInput
	}
	var journal Journal
	if err := readJSON(m.journalPath(operationID), &journal); err != nil {
		return Journal{}, err
	}
	if journal.Version != JournalVersion || journal.OperationID != operationID || journal.Plan.OperationID != operationID || journal.Plan.NodeID != m.nodeID || len(journal.Mutations) == 0 || journal.NextMutation < 0 || journal.NextMutation > len(journal.Mutations) {
		return Journal{}, ErrIntegrity
	}
	if !validSHA256(journal.IntegritySHA256) || journalDigest(journal) != journal.IntegritySHA256 {
		return Journal{}, ErrIntegrity
	}
	for index, mutation := range journal.Mutations {
		if mutation.Index != index {
			return Journal{}, ErrIntegrity
		}
		if mutation.HadOriginal != validSHA256(mutation.OriginalSHA256) || !mutation.HadOriginal && mutation.OriginalSHA256 != "" {
			return Journal{}, ErrIntegrity
		}
		if _, _, _, err := m.mutationPaths(journal, mutation); err != nil {
			return Journal{}, err
		}
	}
	return journal, nil
}

func (m *Manager) writeJournal(journal Journal) error {
	if !operationIDPattern.MatchString(journal.OperationID) || journal.Version != JournalVersion {
		return ErrInvalidInput
	}
	journal.IntegritySHA256 = journalDigest(journal)
	return writeJSONAtomic(m.journalPath(journal.OperationID), journal, 0o600)
}

func journalDigest(journal Journal) string {
	journal.IntegritySHA256 = ""
	encoded, _ := json.Marshal(journal)
	return shaBytes(encoded)
}

func (m *Manager) ensureNoActiveJournal(operationID string) error {
	items, err := os.ReadDir(filepath.Join(m.stateRoot, "journals"))
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		activeOperationID := strings.TrimSuffix(item.Name(), ".json")
		if activeOperationID != operationID {
			if issue := m.recoveryIssues[activeOperationID]; issue != "" {
				return fmt.Errorf("%w: 操作 %s 需要先恢复: %s", ErrOperationInProgress, activeOperationID, issue)
			}
			return fmt.Errorf("%w: operation %s", ErrOperationInProgress, activeOperationID)
		}
	}
	return nil
}

func (m *Manager) writeInstallationStates(ctx context.Context, plan Plan) error {
	states, err := m.readStates()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if states == nil {
		states = make(map[string]InstallationState)
	}
	if plan.Mode == PlanModeContent {
		for _, installation := range plan.Installations {
			if err := ctx.Err(); err != nil {
				return err
			}
			state, exists := states[installation.InstallationID]
			if !exists {
				state = InstallationState{
					InstallationID:     installation.InstallationID,
					Mods:               make(map[string]string),
					ManagedSetupSHA256: shaBytes(installation.ManagedSetup),
					Shards:             make(map[string]ShardState),
				}
			}
			if state.Mods == nil {
				state.Mods = make(map[string]string)
			}
			if state.Shards == nil {
				state.Shards = make(map[string]ShardState)
			}
			for _, mod := range installation.Mods {
				if _, managed := state.Mods[mod.WorkshopID]; managed {
					state.Mods[mod.WorkshopID] = mod.TreeSHA256
				}
				for key, shard := range state.Shards {
					changed := false
					for index := range shard.Mods {
						if shard.Mods[index].WorkshopID == mod.WorkshopID {
							shard.Mods[index] = mod
							changed = true
						}
					}
					if changed {
						state.Shards[key] = shard
					}
				}
			}
			state.LastOperationID = plan.OperationID
			state.WorkshopManifestSHA256 = shaBytes(installation.ManagedWorkshopManifest)
			state.UpdatedAt = time.Now().UTC()
			states[installation.InstallationID] = state
		}
		return writeJSONAtomic(m.statePath(), states, 0o600)
	}
	for _, installation := range plan.Installations {
		if err := ctx.Err(); err != nil {
			return err
		}
		state := InstallationState{
			InstallationID: installation.InstallationID, LastOperationID: plan.OperationID,
			Mods: make(map[string]string, len(installation.Mods)), ManagedSetupSHA256: shaBytes(installation.ManagedSetup),
			WorkshopManifestSHA256: shaBytes(installation.ManagedWorkshopManifest),
			Shards:                 make(map[string]ShardState, len(installation.Shards)),
			UpdatedAt:              time.Now().UTC(),
		}
		for _, mod := range installation.Mods {
			state.Mods[mod.WorkshopID] = mod.TreeSHA256
		}
		for _, shard := range installation.Shards {
			key := shard.RoomID + "/" + shard.WorldID
			state.Shards[key] = ShardState{
				RoomID: shard.RoomID, RoomDirectory: shard.RoomDirectory,
				WorldID: shard.WorldID, WorldDirectory: shard.WorldDirectory,
				Mods: append([]ModVersion(nil), shard.Mods...), ConfigSHA256: shaBytes(shard.ModOverrides),
			}
		}
		states[installation.InstallationID] = state
	}
	return writeJSONAtomic(m.statePath(), states, 0o600)
}

func (m *Manager) State(installationID string) (InstallationState, error) {
	if _, exists := m.installations[installationID]; !exists {
		return InstallationState{}, ErrInvalidInput
	}
	states, err := m.readStates()
	if errors.Is(err, os.ErrNotExist) {
		return InstallationState{}, ErrNotFound
	}
	if err != nil {
		return InstallationState{}, err
	}
	state, exists := states[installationID]
	if !exists {
		return InstallationState{}, ErrNotFound
	}
	if state.InstallationID != installationID || !operationIDPattern.MatchString(state.LastOperationID) {
		return InstallationState{}, ErrIntegrity
	}
	return state, nil
}

// ObserveState verifies the inexpensive parts of the last committed release.
// Mod directories are walked for type/symlink safety, while content identity is
// taken from the tree hash proven during the atomic publish.
func (m *Manager) ObserveState(ctx context.Context, installationID string) (InstallationState, error) {
	state, err := m.State(installationID)
	if err != nil {
		return InstallationState{}, err
	}
	installation := m.installations[installationID]
	for workshopID := range state.Mods {
		if err := ctx.Err(); err != nil {
			return InstallationState{}, err
		}
		exists, inspectErr := inspectTarget(installationModTarget(installation, workshopID), true)
		if inspectErr != nil {
			return InstallationState{}, inspectErr
		}
		if !exists {
			return InstallationState{}, ErrNotFound
		}
	}
	content, readErr := readOptionalRegular(filepath.Join(installation.ServerPath, "mods", "dedicated_server_mods_setup.lua"))
	if readErr != nil {
		return InstallationState{}, readErr
	}
	if shaBytes(content) != state.ManagedSetupSHA256 {
		return InstallationState{}, ErrConflict
	}
	if state.WorkshopManifestSHA256 != "" && installation.WorkshopManifestPath != "" {
		content, readErr = readOptionalRegular(installation.WorkshopManifestPath)
		if readErr != nil {
			return InstallationState{}, readErr
		}
		if shaBytes(content) != state.WorkshopManifestSHA256 {
			return InstallationState{}, ErrConflict
		}
		if inventoryErr := validateWorkshopManifestInventory(content, state.Mods); inventoryErr != nil {
			return InstallationState{}, inventoryErr
		}
	}
	for _, shard := range state.Shards {
		content, readErr := readOptionalRegular(filepath.Join(installation.SavePath, shard.RoomDirectory, shard.WorldDirectory, "modoverrides.lua"))
		if readErr != nil {
			return InstallationState{}, readErr
		}
		if shaBytes(content) != shard.ConfigSHA256 {
			return InstallationState{}, ErrConflict
		}
	}
	return state, nil
}

func (m *Manager) readStates() (map[string]InstallationState, error) {
	states := make(map[string]InstallationState)
	if err := readJSON(m.statePath(), &states); err != nil {
		return nil, err
	}
	for id, state := range states {
		if id != state.InstallationID || !validIdentity(id) || !operationIDPattern.MatchString(state.LastOperationID) {
			return nil, ErrIntegrity
		}
	}
	return states, nil
}

func (m *Manager) statesCommitted(plan Plan) bool {
	for _, installation := range plan.Installations {
		state, err := m.State(installation.InstallationID)
		if err != nil || state.LastOperationID != plan.OperationID {
			return false
		}
	}
	return true
}
