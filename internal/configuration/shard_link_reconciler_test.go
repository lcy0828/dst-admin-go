package configuration

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"dont/internal/rooms"
	"dont/internal/topology"
)

type shardLinkReconcileTopologyStub struct {
	links    []topology.ShardLink
	revision string
	desired  []topology.ShardLink
	commits  int
	pending  bool
}

func (s *shardLinkReconcileTopologyStub) ShardLinkPlan(context.Context, string) (topology.ShardLinkPlan, error) {
	desired := s.desired
	if desired == nil {
		desired = s.links
	}
	return topology.ShardLinkPlan{
		Revision: s.revision, Desired: append([]topology.ShardLink(nil), desired...),
		Applied: append([]topology.ShardLink(nil), s.links...), PlacementPending: s.pending,
	}, nil
}

func (s *shardLinkReconcileTopologyStub) VerifyDesiredShardLinks(context.Context, string) error {
	return nil
}

func (s *shardLinkReconcileTopologyStub) CommitDesiredShardLinks(_ string, revision string) (string, error) {
	if revision != s.revision {
		return "", &topology.RevisionConflictError{CurrentRevision: s.revision}
	}
	s.links = append([]topology.ShardLink(nil), s.desired...)
	s.commits++
	s.revision = "revision-2"
	return s.revision, nil
}

type shardLinkPublisherStub struct {
	requests []PublicationRequest
	err      error
}

func (s *shardLinkPublisherStub) Publish(_ context.Context, request PublicationRequest) (PublicationResult, error) {
	s.requests = append(s.requests, request)
	return PublicationResult{PublishedCount: 2}, s.err
}

type shardLinkRefresherStub struct {
	mu    sync.Mutex
	calls []string
	err   error
}

type shardLinkConfigurationSourceStub struct {
	payload []rooms.ProvisionFile
	err     error
}

func (s shardLinkConfigurationSourceStub) SharedConfigurationPayload(context.Context, string) ([]rooms.ProvisionFile, error) {
	return append([]rooms.ProvisionFile(nil), s.payload...), s.err
}

func (s *shardLinkRefresherStub) RefreshRuntimeTarget(_ context.Context, targetID, installationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, targetID+"/"+installationID)
	return s.err
}

func (s *shardLinkRefresherStub) recordedCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]string(nil), s.calls...)
	sort.Strings(result)
	return result
}

func shardLinkReconcileFixture() (*ShardLinkReconciler, *shardLinkReconcileTopologyStub, *shardLinkPublisherStub, *shardLinkRefresherStub) {
	topologyStub := &shardLinkReconcileTopologyStub{
		revision: "revision-1",
		links: []topology.ShardLink{{
			SourceTargetID: "agent:debian", SourceInstallationID: "native",
			MasterTargetID: "local", MasterInstallationID: "default",
			Address: "192.168.2.24", Port: 10888, Mode: topology.ShardLinkLAN,
		}},
	}
	publisher := &shardLinkPublisherStub{}
	refresher := &shardLinkRefresherStub{}
	source := shardLinkConfigurationSourceStub{payload: []rooms.ProvisionFile{{Name: "cluster.ini", Data: []byte("[SHARD]\nmaster_port = 10888\n"), Mode: 0o640}}}
	reconciler, _ := NewShardLinkReconciler(topologyStub, publisher, refresher, source)
	return reconciler, topologyStub, publisher, refresher
}

func TestShardLinkReconcilerAppliesDesiredRouteWithoutLifecycleAction(t *testing.T) {
	reconciler, topologyStub, publisher, refresher := shardLinkReconcileFixture()
	topologyStub.desired = []topology.ShardLink{{
		SourceTargetID: "agent:debian", SourceInstallationID: "native",
		MasterTargetID: "local", MasterInstallationID: "default",
		Address: "192.168.2.42", Port: 10888, Mode: topology.ShardLinkLAN,
	}}
	if err := reconciler.ApplyDesiredConfiguration(context.Background(), "room-1", "revision-1"); err != nil {
		t.Fatal(err)
	}
	if topologyStub.commits != 1 || topologyStub.revision != "revision-2" || len(publisher.requests) != 1 {
		t.Fatalf("commits=%d revision=%s publications=%d", topologyStub.commits, topologyStub.revision, len(publisher.requests))
	}
	request := publisher.requests[0]
	if !request.UseShardLinks || !reflect.DeepEqual(request.ShardLinks, topologyStub.desired) {
		t.Fatalf("publication request=%#v", request)
	}
	if calls := refresher.recordedCalls(); !reflect.DeepEqual(calls, []string{"agent:debian/native", "local/default"}) {
		t.Fatalf("refreshes=%v", calls)
	}
}

func TestShardLinkReconcilerDoesNotApplyFutureRouteWhilePlacementIsPending(t *testing.T) {
	reconciler, topologyStub, publisher, _ := shardLinkReconcileFixture()
	topologyStub.pending = true
	topologyStub.desired = []topology.ShardLink{{
		SourceTargetID: "agent:new", SourceInstallationID: "native",
		MasterTargetID: "local", MasterInstallationID: "default",
		Address: "192.168.2.50", Port: 10888, Mode: topology.ShardLinkLAN,
	}}
	err := reconciler.ApplyDesiredConfiguration(context.Background(), "room-1", "revision-1")
	var execution *topology.ExecutionError
	if !errors.As(err, &execution) || execution.Code != "PLACEMENT_PENDING" || len(publisher.requests) != 0 || topologyStub.commits != 0 {
		t.Fatalf("execution=%#v publications=%d commits=%d err=%v", execution, len(publisher.requests), topologyStub.commits, err)
	}
}
