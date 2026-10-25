package modcontrol

import (
	"context"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/shared"
	"fmt"
	"path/filepath"
	"time"
)

func (s *Service) validateLocalAddition(ctx context.Context, request proposal) error {
	if s.runtimeFiles == nil {
		return mods.ErrModNotDownloaded
	}
	targets, err := s.selectedInstallationPlacements(ctx, request.roomID, request.worldIDs)
	if err != nil {
		return err
	}
	ctx = withPublicationSnapshot(ctx, proposal{roomID: request.roomID, roomOnly: true, readOnly: true})
	for _, target := range targets {
		observed, err := s.runtimeFiles.ObserveRuntimeModFiles(ctx, RuntimeFileRequest{TargetID: target.TargetID, InstallationID: target.InstallationID, TopologyRevision: "local-mod-install-v1", WorkshopIDs: []string{request.modID}})
		if err != nil {
			return err
		}
		if observed == nil {
			return mods.ErrModNotDownloaded
		}
		state, exists := observed.Mods[request.modID]
		if !exists || state.Status != shared.RuntimeModFileReady || state.Reason != "" {
			return fmt.Errorf("%s/%s: %w", target.TargetID, target.InstallationID, mods.ErrModNotDownloaded)
		}
	}
	return nil
}

func (s *Service) LocalModDetails(ctx context.Context, targetID, installationID, id string) (mods.SteamMod, error) {
	reader, ok := s.runtimeFiles.(interface {
		LocalModDetails(context.Context, string, string, string) (mods.SteamMod, error)
	})
	if !ok {
		return mods.SteamMod{}, runtimedriver.ErrCapabilityMissing
	}
	return reader.LocalModDetails(ctx, targetID, installationID, id)
}

// Loaded means observed in the log for a currently observed process. A stopped
// world's old log, an offline node or a previous boot cannot supply that fact.
func (s *Service) currentModLog(ctx context.Context, room rooms.Room, request RuntimeFileRequest, world shared.RuntimeModWorldFileState) bool {
	if !world.LogObserved || world.LogStartedAt.IsZero() {
		return false
	}
	execution, exists, err := s.source.Execution(ctx, request.TargetID, request.InstallationID)
	if err != nil || !exists || !execution.Inventory.Available || execution.Inventory.Stale {
		return false
	}
	shard := ""
	for _, item := range request.Worlds {
		if item.WorldID == world.WorldID {
			shard = item.WorldDirectory
			break
		}
	}
	if shard == "" {
		return false
	}
	saveRoot := ""
	for _, installation := range execution.Target.Installations {
		if installation.ID == request.InstallationID {
			saveRoot = installation.SavePath
			break
		}
	}
	if saveRoot == "" && request.TargetID == "local" && request.InstallationID == "default" {
		saveRoot = s.source.saveRoot
	}
	for _, process := range execution.Inventory.Inventory.Processes {
		if process.PID <= 0 || process.Cluster != room.DirectoryName || process.Shard != shard || process.StartedAt == nil || process.StartedAt.IsZero() {
			continue
		}
		if saveRoot == "" || process.StorageRoot == "" || filepath.Clean(process.StorageRoot) != filepath.Clean(saveRoot) {
			continue
		}
		delta := world.LogStartedAt.Sub(*process.StartedAt)
		if delta >= -time.Second && delta <= 2*time.Minute {
			return true
		}
	}
	return false
}
