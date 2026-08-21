package moddistribution

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

func (m *Manager) Prepare(ctx context.Context, plan Plan) (Journal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureNoActiveJournal(plan.OperationID); err != nil {
		return Journal{}, err
	}
	canonical, err := m.rebuildPlan(ctx, plan)
	if err != nil {
		return Journal{}, err
	}
	if existing, err := m.readJournal(plan.OperationID); err == nil {
		if existing.Phase == PhasePrepared || existing.Phase == PhasePublished {
			return existing, nil
		}
		return Journal{}, ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return Journal{}, err
	}
	mutations, err := m.buildMutations(ctx, canonical)
	if err != nil {
		return Journal{}, err
	}
	now := time.Now().UTC()
	journal := Journal{
		Version: JournalVersion, OperationID: canonical.OperationID, Phase: PhasePreparing,
		Plan: canonical, Mutations: mutations, CreatedAt: now, UpdatedAt: now,
	}
	if err := m.writeJournal(journal); err != nil {
		return Journal{}, err
	}
	for _, mutation := range journal.Mutations {
		if err := ctx.Err(); err != nil {
			_ = m.rollbackJournal(context.Background(), &journal)
			return Journal{}, err
		}
		if err := m.stageMutation(ctx, journal, mutation); err != nil {
			_ = m.rollbackJournal(context.Background(), &journal)
			return Journal{}, err
		}
	}
	journal.Phase = PhasePrepared
	journal.UpdatedAt = time.Now().UTC()
	if err := m.writeJournal(journal); err != nil {
		_ = m.rollbackJournal(context.Background(), &journal)
		return Journal{}, err
	}
	return journal, nil
}

func (m *Manager) Publish(ctx context.Context, operationID string) (Journal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := m.readJournal(operationID)
	if err != nil {
		return Journal{}, err
	}
	if journal.Phase == PhasePublished {
		return journal, nil
	}
	if journal.Phase != PhasePrepared && journal.Phase != PhasePublishing {
		return Journal{}, ErrConflict
	}
	journal.Phase = PhasePublishing
	journal.UpdatedAt = time.Now().UTC()
	if err := m.writeJournal(journal); err != nil {
		return Journal{}, err
	}
	for index := journal.NextMutation; index < len(journal.Mutations); index++ {
		if err := ctx.Err(); err != nil {
			return journal, err
		}
		mutation := journal.Mutations[index]
		if err := m.publishMutation(journal, mutation); err != nil {
			return journal, err
		}
		journal.NextMutation = index + 1
		journal.UpdatedAt = time.Now().UTC()
		if err := m.writeJournal(journal); err != nil {
			return journal, err
		}
	}
	journal.Phase = PhasePublished
	journal.UpdatedAt = time.Now().UTC()
	if err := m.writeJournal(journal); err != nil {
		return Journal{}, err
	}
	return journal, nil
}

func (m *Manager) Complete(ctx context.Context, operationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := m.readJournal(operationID)
	if errors.Is(err, os.ErrNotExist) {
		committed, stateErr := m.operationCommitted(operationID)
		if stateErr != nil {
			return stateErr
		}
		if committed {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if journal.Phase != PhasePublished && journal.Phase != PhaseCompleting && journal.Phase != PhaseCommitted {
		return ErrConflict
	}
	if journal.Phase != PhaseCommitted {
		journal.Phase = PhaseCompleting
		journal.UpdatedAt = time.Now().UTC()
		if err := m.writeJournal(journal); err != nil {
			return err
		}
		if err := m.verifyPublished(ctx, journal); err != nil {
			return err
		}
		if err := m.writeInstallationStates(ctx, journal.Plan); err != nil {
			return err
		}
		journal.Phase = PhaseCommitted
		journal.UpdatedAt = time.Now().UTC()
		if err := m.writeJournal(journal); err != nil {
			return err
		}
	}
	if err := m.cleanupJournalArtifacts(journal); err != nil {
		return err
	}
	return os.Remove(m.journalPath(operationID))
}

func (m *Manager) operationCommitted(operationID string) (bool, error) {
	states, err := m.readStates()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, state := range states {
		if state.LastOperationID == operationID {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) Rollback(ctx context.Context, operationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := m.readJournal(operationID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.rollbackJournal(ctx, &journal)
}

func (m *Manager) Apply(ctx context.Context, plan Plan) error {
	if _, err := m.Prepare(ctx, plan); err != nil {
		return err
	}
	if _, err := m.Publish(ctx, plan.OperationID); err != nil {
		_ = m.Rollback(context.Background(), plan.OperationID)
		return err
	}
	if err := m.Complete(ctx, plan.OperationID); err != nil {
		return err
	}
	return nil
}

func (m *Manager) buildMutations(ctx context.Context, plan Plan) ([]Mutation, error) {
	mutations := make([]Mutation, 0)
	for _, installationPlan := range plan.Installations {
		installation := m.installations[installationPlan.InstallationID]
		for _, mod := range installationPlan.Mods {
			target := installationModTarget(installation, mod.WorkshopID)
			hadOriginal, err := inspectTarget(target, true)
			if err != nil {
				return nil, err
			}
			originalSHA, err := targetDigest(ctx, target, true, hadOriginal)
			if err != nil {
				return nil, err
			}
			mutations = append(mutations, Mutation{
				Kind: MutationMod, InstallationID: installationPlan.InstallationID,
				WorkshopID: mod.WorkshopID, TreeSHA256: mod.TreeSHA256, HadOriginal: hadOriginal, OriginalSHA256: originalSHA,
			})
		}
		if installation.WorkshopContentPath == "" {
			setupTarget := filepath.Join(installation.ServerPath, "mods", "dedicated_server_mods_setup.lua")
			setupOriginal, err := inspectTarget(setupTarget, false)
			if err != nil {
				return nil, err
			}
			setupOriginalSHA, err := targetDigest(ctx, setupTarget, false, setupOriginal)
			if err != nil {
				return nil, err
			}
			if setupOriginalSHA != installationPlan.SetupBaseSHA256 {
				return nil, ErrConflict
			}
			mutations = append(mutations, Mutation{
				Kind: MutationSetup, InstallationID: installationPlan.InstallationID,
				ConfigSHA256: shaBytes(installationPlan.ManagedSetup), HadOriginal: setupOriginal, OriginalSHA256: setupOriginalSHA,
			})
		}
		for _, shard := range installationPlan.Shards {
			target := filepath.Join(installation.SavePath, shard.RoomDirectory, shard.WorldDirectory, "modoverrides.lua")
			hadOriginal, err := inspectTarget(target, false)
			if err != nil {
				return nil, err
			}
			originalSHA, err := targetDigest(ctx, target, false, hadOriginal)
			if err != nil {
				return nil, err
			}
			mutations = append(mutations, Mutation{
				Kind: MutationOverrides, InstallationID: installationPlan.InstallationID,
				RoomDirectory: shard.RoomDirectory, WorldDirectory: shard.WorldDirectory,
				ConfigSHA256: shaBytes(shard.ModOverrides), HadOriginal: hadOriginal, OriginalSHA256: originalSHA,
			})
		}
	}
	for index := range mutations {
		mutations[index].Index = index
	}
	return mutations, nil
}

func (m *Manager) stageMutation(ctx context.Context, journal Journal, mutation Mutation) error {
	target, stage, backup, err := m.mutationPaths(journal, mutation)
	if err != nil {
		return err
	}
	for _, path := range []string{stage, backup} {
		if _, err := os.Lstat(path); err == nil {
			return ErrConflict
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(filepath.Dir(target)); err != nil {
		return err
	}
	if mutation.Kind == MutationMod {
		manifest, err := m.Verify(ctx, mutation.WorkshopID, mutation.TreeSHA256)
		if err != nil {
			return err
		}
		if err := ensureDiskSpace(filepath.Dir(target), manifest.Size, m.reserveBytes); err != nil {
			return err
		}
		if err := copyManifestTree(ctx, filepath.Join(m.cacheVersionRoot(mutation.WorkshopID, mutation.TreeSHA256), "content"), stage, manifest); err != nil {
			return err
		}
		// macOS requires the directory itself to be writable while it is
		// renamed. It is sealed again immediately after atomic publication.
		return os.Chmod(stage, 0o700)
	}
	content, err := findMutationContent(journal.Plan, mutation)
	if err != nil {
		return err
	}
	if shaBytes(content) != mutation.ConfigSHA256 {
		return ErrIntegrity
	}
	if err := ensureDiskSpace(filepath.Dir(target), int64(len(content)), m.reserveBytes); err != nil {
		return err
	}
	file, err := os.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if written != len(content) {
		return ErrIntegrity
	}
	return syncDirectory(filepath.Dir(stage))
}

func (m *Manager) publishMutation(journal Journal, mutation Mutation) error {
	target, stage, backup, err := m.mutationPaths(journal, mutation)
	if err != nil {
		return err
	}
	stageExists, err := inspectTarget(stage, mutation.Kind == MutationMod)
	if err != nil {
		return err
	}
	backupExists, err := inspectTarget(backup, mutation.Kind == MutationMod)
	if err != nil {
		return err
	}
	targetExists, err := inspectTarget(target, mutation.Kind == MutationMod)
	if err != nil {
		return err
	}
	if !stageExists {
		if targetExists && (backupExists || !mutation.HadOriginal) {
			return nil
		}
		return ErrConflict
	}
	if mutation.HadOriginal && !backupExists {
		if !targetExists {
			return ErrConflict
		}
		currentSHA, err := targetDigest(context.Background(), target, mutation.Kind == MutationMod, true)
		if err != nil {
			return err
		}
		if currentSHA != mutation.OriginalSHA256 {
			return ErrConflict
		}
		if err := renameMutationTarget(target, backup, mutation.Kind == MutationMod); err != nil {
			return err
		}
		targetExists = false
	}
	if !mutation.HadOriginal && targetExists {
		return ErrConflict
	}
	if targetExists {
		return ErrConflict
	}
	if err := renameMutationTarget(stage, target, mutation.Kind == MutationMod); err != nil {
		return err
	}
	if mutation.Kind == MutationMod {
		if err := os.Chmod(target, 0o555); err != nil {
			return err
		}
	}
	return syncDirectory(filepath.Dir(target))
}

func (m *Manager) rollbackJournal(ctx context.Context, journal *Journal) error {
	journal.Phase = PhaseRollingBack
	journal.UpdatedAt = time.Now().UTC()
	if err := m.writeJournal(*journal); err != nil {
		return err
	}
	var result error
	for index := len(journal.Mutations) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		mutation := journal.Mutations[index]
		target, stage, backup, err := m.mutationPaths(*journal, mutation)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		backupExists, inspectErr := inspectTarget(backup, mutation.Kind == MutationMod)
		if inspectErr != nil {
			result = errors.Join(result, inspectErr)
			continue
		}
		stageExists, inspectErr := inspectTarget(stage, mutation.Kind == MutationMod)
		if inspectErr != nil {
			result = errors.Join(result, inspectErr)
			continue
		}
		if backupExists {
			if err := removeTarget(target, mutation.Kind == MutationMod); err != nil {
				result = errors.Join(result, err)
				continue
			}
			if err := renameMutationTarget(backup, target, mutation.Kind == MutationMod); err != nil {
				result = errors.Join(result, err)
				continue
			}
		} else if !mutation.HadOriginal && !stageExists {
			result = errors.Join(result, removeTarget(target, mutation.Kind == MutationMod))
		}
		result = errors.Join(result, removeTarget(stage, mutation.Kind == MutationMod))
	}
	if result != nil {
		return result
	}
	if err := m.cleanupJournalArtifacts(*journal); err != nil {
		return err
	}
	return os.Remove(m.journalPath(journal.OperationID))
}

func (m *Manager) cleanupJournalArtifacts(journal Journal) error {
	var result error
	for _, mutation := range journal.Mutations {
		_, stage, backup, err := m.mutationPaths(journal, mutation)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		result = errors.Join(result, removeTarget(stage, mutation.Kind == MutationMod), removeTarget(backup, mutation.Kind == MutationMod))
	}
	return result
}

func (m *Manager) verifyPublished(ctx context.Context, journal Journal) error {
	for _, mutation := range journal.Mutations {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, _, _, err := m.mutationPaths(journal, mutation)
		if err != nil {
			return err
		}
		if mutation.Kind == MutationMod {
			manifest, err := m.Verify(ctx, mutation.WorkshopID, mutation.TreeSHA256)
			if err != nil {
				return err
			}
			entries, size, err := scanTree(ctx, target)
			if err != nil || size != manifest.Size || hashEntries(entries) != manifest.TreeSHA256 || !sameEntries(entries, manifest.Entries) {
				return errors.Join(ErrIntegrity, err)
			}
			continue
		}
		content, err := os.ReadFile(target)
		if err != nil || shaBytes(content) != mutation.ConfigSHA256 {
			return errors.Join(ErrIntegrity, err)
		}
	}
	return nil
}

func (m *Manager) mutationPaths(journal Journal, mutation Mutation) (string, string, string, error) {
	if journal.OperationID == "" || mutation.Index < 0 || mutation.Index >= len(journal.Mutations) || journal.Mutations[mutation.Index] != mutation {
		return "", "", "", ErrIntegrity
	}
	installation, exists := m.installations[mutation.InstallationID]
	if !exists {
		return "", "", "", ErrUnsafePath
	}
	suffix := fmt.Sprintf("%s-%03d", journal.OperationID, mutation.Index)
	var target, stage, backup string
	if mutation.Kind == MutationMod && validWorkshopID(mutation.WorkshopID) && validSHA256(mutation.TreeSHA256) {
		parent := filepath.Dir(installationModTarget(installation, mutation.WorkshopID))
		target = installationModTarget(installation, mutation.WorkshopID)
		stage = filepath.Join(parent, ".dst-admin-mod-stage-"+suffix)
		backup = filepath.Join(parent, ".dst-admin-mod-backup-"+suffix)
	} else if mutation.Kind == MutationSetup && validSHA256(mutation.ConfigSHA256) {
		parent := filepath.Join(installation.ServerPath, "mods")
		target = filepath.Join(parent, "dedicated_server_mods_setup.lua")
		stage = filepath.Join(parent, ".dst-admin-setup-stage-"+suffix)
		backup = filepath.Join(parent, ".dst-admin-setup-backup-"+suffix)
	} else if mutation.Kind == MutationOverrides && safeComponent(mutation.RoomDirectory) && safeComponent(mutation.WorldDirectory) && validSHA256(mutation.ConfigSHA256) {
		parent := filepath.Join(installation.SavePath, mutation.RoomDirectory, mutation.WorldDirectory)
		target = filepath.Join(parent, "modoverrides.lua")
		stage = filepath.Join(parent, ".dst-admin-overrides-stage-"+suffix)
		backup = filepath.Join(parent, ".dst-admin-overrides-backup-"+suffix)
	} else {
		return "", "", "", ErrIntegrity
	}
	trustedRoot := installation.SavePath
	if mutation.Kind == MutationMod && installation.WorkshopContentPath != "" {
		trustedRoot = installation.WorkshopContentPath
	} else if mutation.Kind == MutationMod || mutation.Kind == MutationSetup {
		trustedRoot = installation.ServerPath
	}
	for _, path := range []string{target, stage, backup} {
		if !pathWithin(path, trustedRoot) {
			return "", "", "", ErrUnsafePath
		}
	}
	return target, stage, backup, nil
}

func installationModTarget(installation TrustedInstallation, workshopID string) string {
	if installation.WorkshopContentPath != "" {
		return filepath.Join(installation.WorkshopContentPath, workshopID)
	}
	return filepath.Join(installation.ServerPath, "mods", "workshop-"+workshopID)
}

func inspectTarget(path string, directory bool) (bool, error) {
	if err := rejectSymlinkComponents(filepath.Dir(path)); err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || directory != info.IsDir() || !directory && !info.Mode().IsRegular() {
		return false, ErrUnsafePath
	}
	if directory {
		err := filepath.WalkDir(path, func(_ string, item fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if item.Type()&os.ModeSymlink != 0 {
				return ErrUnsafePath
			}
			return nil
		})
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func targetDigest(ctx context.Context, path string, directory, exists bool) (string, error) {
	if !exists {
		return "", nil
	}
	if directory {
		entries, _, err := scanTree(ctx, path)
		if err != nil {
			return "", err
		}
		return hashEntries(entries), nil
	}
	return hashFile(ctx, path)
}

func removeTarget(path string, directory bool) error {
	exists, err := inspectTarget(path, directory)
	if err != nil || !exists {
		return err
	}
	if directory {
		if err := makeTreeWritable(path); err != nil {
			return err
		}
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

func renameMutationTarget(source, destination string, directory bool) error {
	if !directory {
		return os.Rename(source, destination)
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	originalMode := info.Mode().Perm()
	modeChanged := originalMode&0o200 == 0
	if modeChanged {
		// macOS refuses to rename a read-only directory even when its parent is
		// writable. Keep the writable window limited to the atomic rename.
		if err := os.Chmod(source, originalMode|0o200); err != nil {
			return err
		}
	}
	if err := os.Rename(source, destination); err != nil {
		if modeChanged {
			return errors.Join(err, os.Chmod(source, originalMode))
		}
		return err
	}
	if modeChanged {
		return os.Chmod(destination, originalMode)
	}
	return nil
}

func makeTreeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.Type()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		if item.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	})
}

func findMutationContent(plan Plan, mutation Mutation) ([]byte, error) {
	for _, installation := range plan.Installations {
		if installation.InstallationID != mutation.InstallationID {
			continue
		}
		if mutation.Kind == MutationSetup {
			return append([]byte(nil), installation.ManagedSetup...), nil
		}
		for _, shard := range installation.Shards {
			if shard.RoomDirectory == mutation.RoomDirectory && shard.WorldDirectory == mutation.WorldDirectory {
				return append([]byte(nil), shard.ModOverrides...), nil
			}
		}
	}
	return nil, ErrIntegrity
}

func (m *Manager) rebuildPlan(ctx context.Context, plan Plan) (Plan, error) {
	input := PlanInput{OperationID: plan.OperationID, NodeID: plan.NodeID}
	for _, installation := range plan.Installations {
		if installation.InstallationID == "" || installation.NodeID != plan.NodeID {
			return Plan{}, ErrInvalidInput
		}
		for _, shard := range installation.Shards {
			if shard.InstallationID != installation.InstallationID {
				return Plan{}, ErrInvalidInput
			}
			input.Shards = append(input.Shards, shard)
		}
	}
	canonical, err := m.BuildPlan(ctx, input)
	if err != nil {
		return Plan{}, err
	}
	canonical.CreatedAt = plan.CreatedAt
	if canonical.CreatedAt.IsZero() {
		canonical.CreatedAt = time.Now().UTC()
	}
	return canonical, nil
}
