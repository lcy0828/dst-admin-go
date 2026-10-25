package saveimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"dont/internal/distributedbackup"
	"dont/internal/rooms"
	"dont/internal/runtimefiles"

	"github.com/go-ini/ini"
)

func (s *Service) snapshotImportTarget(ctx context.Context, id string, room rooms.Room, preserveToken bool) (string, map[string][]byte, func(), error) {
	noop := func() {}
	if s.runtimeReader == nil {
		// Legacy in-process callers may use an explicitly local catalog. A
		// configured reader must never fall back to Controller shadow files.
		if s.guard != nil {
			if err := s.guard.RequireRoom(room.ID); err != nil {
				return "", nil, noop, err
			}
		}
		root := filepath.Join(s.config.SaveRoot, room.DirectoryName)
		if !contained(s.config.SaveRoot, root) || !regularDirectory(root) {
			return "", nil, noop, ErrTargetNotManaged
		}
		return root, nil, noop, nil
	}
	worlds, err := s.rooms.Worlds(room.ID)
	if err != nil || len(worlds) == 0 {
		if err != nil {
			return "", nil, noop, err
		}
		return "", nil, noop, ErrWorldMismatch
	}
	root, err := os.MkdirTemp(filepath.Join(s.config.ImportRoot, id), ".target-")
	if err != nil {
		return "", nil, noop, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()
	sharedConfigs := make(map[string][]byte, len(worlds))
	sharedByTarget := make(map[string][]byte)
	var token string
	tokenKnown := false
	for index, world := range worlds {
		// Room catalog names are logical directories, never arbitrary paths.
		if !directoryNamePattern.MatchString(world.DirectoryName) {
			return "", nil, noop, ErrWorldMismatch
		}
		worldConfig, err := s.runtimeReader.ReadConfiguration(ctx, room.ID, world.ID, runtimefiles.ConfigurationScopeWorld)
		if err != nil {
			return "", nil, noop, fmt.Errorf("read target %s configuration: %w", world.Name, err)
		}
		if err := runtimefiles.ValidateConfiguration(runtimefiles.ConfigurationScopeWorld, worldConfig.Result); err != nil {
			return "", nil, noop, err
		}
		key := worldConfig.Target.TargetID + "\x00" + worldConfig.Target.InstallationID + "\x00" + worldConfig.Target.Cluster
		cluster, known := sharedByTarget[key]
		if !known {
			sharedConfig, err := s.runtimeReader.ReadConfiguration(ctx, room.ID, world.ID, runtimefiles.ConfigurationScopeShared)
			if err != nil {
				return "", nil, noop, fmt.Errorf("read target %s shared configuration: %w", world.Name, err)
			}
			if err := runtimefiles.ValidateConfiguration(runtimefiles.ConfigurationScopeShared, sharedConfig.Result); err != nil {
				return "", nil, noop, err
			}
			for _, file := range sharedConfig.Result.Files {
				if file.Name == "cluster.ini" {
					cluster = file.Data
				}
			}
			sharedByTarget[key] = cluster
			if preserveToken {
				value, err := s.runtimeReader.RevealClusterToken(ctx, room.ID, world.ID)
				if err != nil {
					return "", nil, noop, err
				}
				if err := runtimefiles.ValidateClusterTokenReveal(value); err != nil {
					return "", nil, noop, err
				}
				if tokenKnown && token != value.Token {
					return "", nil, noop, fmt.Errorf("运行节点的房间令牌不一致，请先同步房间配置")
				}
				token = value.Token
				tokenKnown = true
			}
		}
		sharedConfigs[world.ID] = cluster
		if index == 0 {
			if err := os.WriteFile(filepath.Join(root, "cluster.ini"), cluster, 0600); err != nil {
				return "", nil, noop, err
			}
		}
		worldRoot := filepath.Join(root, world.DirectoryName)
		if err := os.MkdirAll(worldRoot, 0700); err != nil {
			return "", nil, noop, err
		}
		for _, file := range worldConfig.Result.Files {
			if file.Name == "server.ini" {
				if err := os.WriteFile(filepath.Join(worldRoot, "server.ini"), file.Data, 0600); err != nil {
					return "", nil, noop, err
				}
			}
		}
	}
	if preserveToken {
		if err := os.WriteFile(filepath.Join(root, "cluster_token.txt"), []byte(token), 0600); err != nil {
			return "", nil, noop, err
		}
	}
	success = true
	return root, sharedConfigs, cleanup, nil
}

// Make a small shared-files subtree for each Placement, moving each staged
// world once rather than copying potentially large saves a second time.
func preparePlacementSharedFiles(root string, worlds []distributedbackup.DirectoryImportWorld, targetConfigs map[string][]byte) error {
	runtimeRoot, err := os.MkdirTemp(root, ".runtime-worlds-")
	if err != nil {
		return err
	}
	for index := range worlds {
		world := &worlds[index]
		destination := filepath.Join(runtimeRoot, world.DirectoryName)
		if err := os.MkdirAll(destination, 0700); err != nil {
			return err
		}
		for _, name := range []string{"cluster.ini", "cluster_token.txt", "adminlist.txt", "blocklist.txt", "whitelist.txt"} {
			data, err := os.ReadFile(filepath.Join(root, name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(destination, name), data, 0600); err != nil {
				return err
			}
		}
		current, err := ini.Load(targetConfigs[world.WorldID])
		if err != nil {
			return err
		}
		path := filepath.Join(destination, "cluster.ini")
		imported, err := ini.Load(path)
		if err != nil {
			return err
		}
		preserveClusterNetwork(current, imported)
		if err := imported.SaveTo(path); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(root, world.DirectoryName), filepath.Join(destination, world.DirectoryName)); err != nil {
			return err
		}
		world.SourceRoot = destination
	}
	return nil
}
