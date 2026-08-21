package agent

import (
	"context"
	"errors"
	"path/filepath"

	"dont/internal/maptransfer"
	"dont/internal/worldmap"
	"dont/shared"
)

func (a *Agent) observeMapAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	manager, err := a.mapTransferManager(installation)
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "地图数据已读取")
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	response := &shared.RuntimeMapResult{}
	result.Map = response
	switch request.Action {
	case shared.RuntimeActionMapSessions:
		response.Sessions, err = manager.Sessions(request.Cluster, request.Shard)
		response.Complete = err == nil
	case shared.RuntimeActionMapStatus:
		status := manager.RendererStatus()
		response.Renderer = &status
		response.Complete = true
	case shared.RuntimeActionMapRead:
		chunk, readErr := manager.Read(ctx, request.Map.TransferID, request.Map.Offset)
		response.TransferID = chunk.TransferID
		response.Offset, response.NextOffset = chunk.Offset, chunk.NextOffset
		response.Size, response.SHA256, response.SourceSHA256 = chunk.Size, chunk.SHA256, chunk.SourceSHA256
		response.Data, response.Complete, response.Log = chunk.Data, chunk.Complete, chunk.Log
		err = readErr
	default:
		err = errors.New("地图读取动作不受支持")
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func (a *Agent) executeMapAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	manager, err := a.mapTransferManager(installation)
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "地图 Runtime 步骤已完成")
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	value := *request.Map
	response := &shared.RuntimeMapResult{TransferID: value.TransferID}
	result.Map = response
	switch request.Action {
	case shared.RuntimeActionMapSnapshotPrepare:
		descriptor, prepareErr := manager.PrepareSnapshot(ctx, value.TransferID, request.Cluster, request.Shard, value.SessionID, value.FileName)
		fillRuntimeMapDescriptor(response, descriptor)
		response.Complete = prepareErr == nil
		err = prepareErr
	case shared.RuntimeActionMapRender:
		layers := make([]worldmap.Layer, len(value.Layers))
		for index := range value.Layers {
			layers[index] = worldmap.Layer(value.Layers[index])
		}
		descriptor, renderErr := manager.Render(ctx, value.TransferID, request.Cluster, request.Shard, value.SessionID, value.FileName, layers)
		fillRuntimeMapDescriptor(response, descriptor)
		response.Complete = renderErr == nil
		err = renderErr
	case shared.RuntimeActionMapRelease:
		err = manager.Release(value.TransferID)
		response.Complete = err == nil
	default:
		err = errors.New("地图 Runtime 动作不受支持")
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func fillRuntimeMapDescriptor(target *shared.RuntimeMapResult, value maptransfer.Descriptor) {
	target.TransferID = value.TransferID
	target.Size, target.SHA256 = value.Size, value.SHA256
	target.SourceSHA256, target.Log = value.SourceSHA256, value.Log
}

func (a *Agent) mapTransferManager(installation RuntimeInstallation) (*maptransfer.Manager, error) {
	a.mapTransferMu.Lock()
	defer a.mapTransferMu.Unlock()
	if existing := a.mapTransfers[installation.ID]; existing != nil {
		return existing, nil
	}
	if a.mapTransfers == nil {
		a.mapTransfers = make(map[string]*maptransfer.Manager)
	}
	renderer := worldmap.NewExecRenderer("", installation.ServerPath)
	created, err := maptransfer.New(
		installation.SavePath,
		filepath.Join(a.Config.OperationStateFile+".maps", installation.ID),
		renderer,
	)
	if err != nil {
		return nil, err
	}
	a.mapTransfers[installation.ID] = created
	return created, nil
}
