package gameupdate

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"dont/internal/operationprogress"
	"dont/internal/runtimedriver"
	"dont/shared"
)

// ReleaseDownloadProgress describes only SteamCMD's current phase. It is not
// evidence that an installation is verified or that a world has recovered.
type ReleaseDownloadProgress struct {
	OperationID    string    `json:"operationId"`
	Phase          string    `json:"phase"`
	Attempt        int       `json:"attempt"`
	CurrentBytes   int64     `json:"currentBytes"`
	TotalBytes     int64     `json:"totalBytes"`
	BytesPerSecond int64     `json:"bytesPerSecond,omitempty"`
	PhaseStartedAt time.Time `json:"phaseStartedAt"`
	ObservedAt     time.Time `json:"observedAt"`
}

func releaseDownloadPhase(stage string) string {
	switch stage {
	case "game.install", "game.prepare":
		return "preparing"
	case "game.download":
		return "downloading"
	case "game.validate":
		return "validating"
	case "game.finalize":
		return "finalizing"
	default:
		return ""
	}
}

func (s *ReleaseStore) saveDownloadProgress(id, target, installation string, progress *ReleaseDownloadProgress) {
	// Best-effort observability must never interrupt a SteamCMD write. This
	// small update avoids rewriting the plan, logs and all world rows per tick.
	_ = s.db.Table(s.installationTable).
		Where("release_id = ? AND target_id = ? AND installation_id = ? AND stage = ?", id, target, installation, ReleaseStageUpdating).
		UpdateColumn("progress_json", downloadProgressJSON(progress)).Error
}

func downloadProgressJSON(progress *ReleaseDownloadProgress) string {
	if progress == nil {
		return ""
	}
	data, _ := json.Marshal(progress)
	return string(data)
}

func (c *ReleaseCoordinator) updateWithProgress(ctx context.Context, value Release, target ReleaseInstallationPlan, operation runtimedriver.Operation, desired string) (shared.RuntimeGameVersionResult, *ReleaseDownloadProgress, error) {
	var mu sync.Mutex
	var latest *ReleaseDownloadProgress
	closed := false
	progressContext := operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		mu.Lock()
		defer mu.Unlock()
		phase := releaseDownloadPhase(update.Stage)
		if closed || phase == "" || update.CurrentBytes < 0 || update.TotalBytes < 0 || update.CurrentBytes > update.TotalBytes {
			return
		}
		now := c.now().UTC()
		attempt := update.Attempt
		if attempt == 0 {
			attempt = 1
		}
		if attempt < 1 || attempt > 2 || latest != nil && attempt < latest.Attempt {
			return
		}
		changed := latest == nil || phase != latest.Phase || attempt != latest.Attempt
		complete := update.TotalBytes > 0 && update.CurrentBytes == update.TotalBytes &&
			(latest == nil || latest.CurrentBytes != update.CurrentBytes || latest.TotalBytes != update.TotalBytes)
		if !changed && now.Sub(latest.ObservedAt) < time.Second && !complete {
			return
		}
		started := now
		if !changed {
			started = latest.PhaseStartedAt
		}
		latest = &ReleaseDownloadProgress{OperationID: operation.ID, Phase: phase, Attempt: attempt,
			CurrentBytes: update.CurrentBytes, TotalBytes: update.TotalBytes, BytesPerSecond: max(0, update.BytesPerSecond),
			PhaseStartedAt: started, ObservedAt: now}
		if phase != "downloading" {
			latest.BytesPerSecond = 0
		}
		c.store.saveDownloadProgress(value.ID, target.TargetID, target.InstallationID, latest)
	})
	result, err := c.runtime.UpdateInstallation(progressContext, target, operation, desired, value.Plan.Policy.CleanCache)
	// A late transport callback cannot overwrite a finished/retried operation.
	mu.Lock()
	closed = true
	progress := latest
	mu.Unlock()
	return result, progress, err
}
