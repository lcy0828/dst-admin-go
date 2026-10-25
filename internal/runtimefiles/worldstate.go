package runtimefiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dont/internal/saveidentity"
	"dont/shared"
)

// ReadWorldState returns the on-disk outputs and their runtime identity without
// sending console input or waiting for the game to write another sample.
func ReadWorldState(ctx context.Context, saveRoot, cluster, shard string, status shared.ShardRuntimeStatus) (shared.RuntimeWorldStateRead, error) {
	value := shared.RuntimeWorldStateRead{Runtime: status}
	if err := ctx.Err(); err != nil {
		return value, err
	}
	root, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		value.ReadError = err.Error()
		return value, nil
	}
	value.Artifacts, err = ReadArtifacts(ctx, saveRoot, cluster, shard, shared.ArtifactRuntimeWorldState)
	if errors.Is(err, os.ErrNotExist) {
		if status.State == "running" {
			for _, name := range []string{"customcommands.lua", "dst-admin/bootstrap.lua", "dst-admin/manifest.json",
				"save/mod_config_data/dst-admin/runtime/bootstrap.lua",
				"save/mod_config_data/dst-admin/runtime/telemetry.lua",
				"save/mod_config_data/dst-admin/runtime/worldstate.lua",
				"save/mod_config_data/dst-admin/runtime/commands.lua",
				"save/mod_config_data/dst-admin/runtime/events.lua",
				"save/mod_config_data/dst-admin/runtime/diagnostics.lua",
				"save/mod_config_data/dst-admin/runtime/barriers.lua"} {
				info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
				if statErr != nil || !info.Mode().IsRegular() {
					value.ReadError = "状态采集脚本尚未安装或不完整，请在运行时管理中安装并激活采集脚本"
					break
				}
			}
		}
		// A world that has never produced JSON has no state to decode. Avoid
		// reading session/configuration/log identity when there is no sample.
		return value, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return value, ctx.Err()
		}
		value.ReadError = fmt.Sprintf("read world state files: %v", err)
		return value, nil
	}
	identity, err := ReadWorldIdentity(ctx, saveRoot, cluster, shard, status)
	value.SessionID, value.ShardID, value.StartedAt = identity.SessionID, identity.ShardID, identity.StartedAt
	if err != nil {
		value.ReadError = fmt.Sprintf("read world state files: %v", err)
	}
	return value, nil
}

// ReadWorldIdentity reads only the current save/config/log identity, even when
// no world-state artifact exists yet. It never sends a console command.
func ReadWorldIdentity(ctx context.Context, saveRoot, cluster, shard string, status shared.ShardRuntimeStatus) (shared.RuntimeWorldStateRead, error) {
	value := shared.RuntimeWorldStateRead{Runtime: status}
	if err := ctx.Err(); err != nil {
		return value, err
	}
	root, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return value, err
	}
	value.SessionID, err = worldSessionID(root)
	if err == nil {
		value.ShardID, err = ReadConfiguredShardID(root)
	}
	if err == nil {
		value.StartedAt, err = worldStartedAt(root)
	}
	return value, err
}

func worldSessionID(root string) (string, error) {
	return saveidentity.CurrentSession(root)
}

func worldStartedAt(root string) (time.Time, error) {
	for _, name := range []string{"server_log.txt", "forest_server_log.txt"} {
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if !info.Mode().IsRegular() {
			return time.Time{}, errors.New("server log is not a regular file")
		}
		file, err := os.Open(path)
		if err != nil {
			return time.Time{}, err
		}
		startedAt := readLogStartTime(file)
		_ = file.Close()
		if !startedAt.IsZero() {
			return startedAt.UTC(), nil
		}
	}
	return time.Time{}, nil
}

// ReadWorldPause observes only engine log records; Lua pause getters do not
// include dedicated-server sleep while empty on every DST build.
func ReadWorldPause(ctx context.Context, saveRoot, cluster, shard string) *bool {
	root, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	for _, name := range []string{"server_log.txt", "forest_server_log.txt"} {
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		start := max(int64(0), info.Size()-simulationScanBytes)
		data := make([]byte, info.Size()-start)
		_, err = file.ReadAt(data, start)
		_ = file.Close()
		if err != nil {
			return nil
		}
		paused, _ := latestSimulationPause(data, start == 0)
		return paused
	}
	return nil
}
