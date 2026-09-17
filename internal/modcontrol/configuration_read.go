package modcontrol

import (
	"context"
	"fmt"

	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
)

func (s *SnapshotSource) readConfiguration(ctx context.Context, roomID, worldID, modID string) (mods.ModConfiguration, []byte, error) {
	if !mods.ValidID(modID) {
		return mods.ModConfiguration{}, nil, mods.ErrInvalidModID
	}
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return mods.ModConfiguration{}, nil, err
	}
	if !room.Managed {
		return mods.ModConfiguration{}, nil, mods.ErrRoomNotManaged
	}
	execution, err := s.topology.ResolveCachedExecution(ctx, roomID, worldID)
	if err != nil {
		return mods.ModConfiguration{}, nil, err
	}
	if execution.Room.ID != roomID || execution.World.ID != worldID {
		return mods.ModConfiguration{}, nil, rooms.ErrWorldNotFound
	}
	content, err := s.readOverrides(ctx, execution, publicationPlacement(execution, roomID, worldID))
	if err != nil {
		return mods.ModConfiguration{}, nil, err
	}
	configuration, err := s.configurationForExecution(ctx, execution, modID, content)
	return configuration, content, err
}

func (s *SnapshotSource) configurationForExecution(ctx context.Context, execution topology.ExecutionPlacement, modID string, content []byte) (mods.ModConfiguration, error) {
	roomID, worldID := execution.Room.ID, execution.World.ID
	placement := publicationPlacement(execution, roomID, worldID)
	if placement.TargetID == "local" {
		return s.mods.ConfigurationFromContent(ctx, roomID, worldID, modID, content)
	}
	if placement.TargetID == "" || placement.InstallationID == "" {
		return mods.ModConfiguration{}, runtimedriver.ErrInvalidTarget
	}
	reader, ok := s.remote.(runtimedriver.ModSchemaReader)
	if !ok {
		return mods.ModConfiguration{}, fmt.Errorf("运行节点不支持 Mod 配置读取，请升级 Agent: %w", runtimedriver.ErrCapabilityMissing)
	}
	schema, err := reader.ReadModSchema(ctx, runtimedriver.Target{
		TargetID: placement.TargetID, InstallationID: placement.InstallationID,
		RoomID: roomID, WorldID: worldID, Cluster: execution.Room.DirectoryName,
		Shard: execution.World.DirectoryName, TopologyRevision: execution.Revision,
	}, modID)
	if err != nil {
		return mods.ModConfiguration{}, fmt.Errorf("%s/%s 的 Mod %s 配置读取失败: %w", placement.TargetID, placement.InstallationID, modID, err)
	}
	return mods.ConfigurationFromParsed(roomID, worldID, modID, content, mods.ParserResult{
		Values: map[string]interface{}{"configuration_options": schema.Options}, Parser: schema.Parser,
		FallbackUsed: schema.FallbackUsed, FallbackReason: schema.FallbackReason, Warnings: schema.Warnings,
	})
}
