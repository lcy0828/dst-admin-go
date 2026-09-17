package configuration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"dont/internal/rooms"
	"dont/internal/topology"
)

var ErrShardLinkConfiguration = errors.New("Shard 互联配置协调失败")

type shardLinkReconcileTopology interface {
	ShardLinkPlan(context.Context, string) (topology.ShardLinkPlan, error)
	VerifyDesiredShardLinks(context.Context, string) error
	CommitDesiredShardLinks(string, string) (string, error)
}

type shardLinkRuntimeRefresher interface {
	RefreshRuntimeTarget(context.Context, string, string) error
}

type shardLinkConfigurationSource interface {
	SharedConfigurationPayload(context.Context, string) ([]rooms.ProvisionFile, error)
}

type ShardLinkReconciler struct {
	topology  shardLinkReconcileTopology
	publisher Publisher
	refresher shardLinkRuntimeRefresher
	source    shardLinkConfigurationSource
}

func NewShardLinkReconciler(topologyService shardLinkReconcileTopology, publisher Publisher, refresher shardLinkRuntimeRefresher, source shardLinkConfigurationSource) (*ShardLinkReconciler, error) {
	if topologyService == nil || publisher == nil || refresher == nil || source == nil {
		return nil, errors.New("Shard link reconciliation dependencies are required")
	}
	return &ShardLinkReconciler{topology: topologyService, publisher: publisher, refresher: refresher, source: source}, nil
}

// ApplyDesiredConfiguration applies a route-only topology change without
// starting or stopping any world. Placement changes remain owned by the
// provisioning and migration coordinators.
func (r *ShardLinkReconciler) ApplyDesiredConfiguration(ctx context.Context, roomID, expectedRevision string) error {
	plan, err := r.topology.ShardLinkPlan(ctx, roomID)
	if err != nil {
		return err
	}
	if expectedRevision != "" && expectedRevision != plan.Revision {
		return &topology.RevisionConflictError{CurrentRevision: plan.Revision}
	}
	if plan.PlacementPending {
		return &topology.ExecutionError{Code: "PLACEMENT_PENDING", Message: "世界运行位置尚未全部生效，不能提前应用计划中的互联线路"}
	}
	if sameShardLinks(plan.Desired, plan.Applied) {
		return nil
	}
	_, err = r.applyDesiredConfiguration(ctx, roomID, plan)
	return err
}

func (r *ShardLinkReconciler) applyDesiredConfiguration(ctx context.Context, roomID string, plan topology.ShardLinkPlan) (string, error) {
	if len(plan.Desired) > 0 {
		if err := r.topology.VerifyDesiredShardLinks(ctx, roomID); err != nil {
			return "", fmt.Errorf("%w: 验证计划互联线路: %v", ErrShardLinkConfiguration, err)
		}
	}
	payload, err := r.source.SharedConfigurationPayload(ctx, roomID)
	if err != nil {
		return "", fmt.Errorf("%w: 从 Master 运行磁盘读取 cluster.ini: %v", ErrShardLinkConfiguration, err)
	}
	if _, err := r.publisher.Publish(ctx, PublicationRequest{
		RoomID: roomID, Scope: PublicationShared, Files: []string{"cluster.ini"}, IncludeLocal: true,
		Payload: payload, ShardLinks: plan.Desired, UseShardLinks: true,
	}); err != nil {
		return "", fmt.Errorf("%w: 应用计划互联线路: %v", ErrShardLinkConfiguration, err)
	}
	revision, err := r.topology.CommitDesiredShardLinks(roomID, plan.Revision)
	if err != nil {
		return "", fmt.Errorf("%w: 提交已生效互联线路: %v", ErrShardLinkConfiguration, err)
	}
	if err := r.refresh(ctx, shardLinkEndpoints(plan.Desired)); err != nil {
		// Runtime publication was read back and topology state is committed. A
		// delayed inventory refresh must not turn a successful apply into failure.
		return revision, nil
	}
	return revision, nil
}

func sameShardLinks(left, right []topology.ShardLink) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type shardLinkEndpoint struct {
	targetID       string
	installationID string
}

func shardLinkEndpoints(links []topology.ShardLink) []shardLinkEndpoint {
	seen := make(map[string]bool, len(links)*2)
	result := make([]shardLinkEndpoint, 0, len(links)+1)
	appendEndpoint := func(targetID, installationID string) {
		targetID, installationID = strings.TrimSpace(targetID), strings.TrimSpace(installationID)
		key := targetID + "\x00" + installationID
		if targetID == "" || installationID == "" || seen[key] {
			return
		}
		seen[key] = true
		result = append(result, shardLinkEndpoint{targetID: targetID, installationID: installationID})
	}
	for _, link := range links {
		appendEndpoint(link.MasterTargetID, link.MasterInstallationID)
		appendEndpoint(link.SourceTargetID, link.SourceInstallationID)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].targetID == result[j].targetID {
			return result[i].installationID < result[j].installationID
		}
		return result[i].targetID < result[j].targetID
	})
	return result
}

func (r *ShardLinkReconciler) refresh(ctx context.Context, endpoints []shardLinkEndpoint) error {
	type refreshFailure struct {
		endpoint shardLinkEndpoint
		err      error
	}
	failures := make(chan refreshFailure, len(endpoints))
	var wait sync.WaitGroup
	for _, endpoint := range endpoints {
		endpoint := endpoint
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := r.refresher.RefreshRuntimeTarget(ctx, endpoint.targetID, endpoint.installationID); err != nil {
				failures <- refreshFailure{endpoint: endpoint, err: err}
			}
		}()
	}
	wait.Wait()
	close(failures)
	values := make([]string, 0, len(failures))
	for failure := range failures {
		values = append(values, fmt.Sprintf("%s/%s: %v", failure.endpoint.targetID, failure.endpoint.installationID, failure.err))
	}
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	return errors.New(strings.Join(values, "；"))
}
