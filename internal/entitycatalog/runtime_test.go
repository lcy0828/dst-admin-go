package entitycatalog

import (
	"context"
	"dont/internal/dstruntime"
	"errors"
	"testing"
	"time"
)

type catalogCommander struct {
	calls       int
	room, world string
	request     dstruntime.CommandRequest
	receipt     dstruntime.CommandReceipt
	err         error
}

func (c *catalogCommander) ExecuteCommand(_ context.Context, room, world string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	c.calls++
	c.room = room
	c.world = world
	c.request = request
	return c.receipt, c.err
}
func TestRuntimeSearchUsesSelectedWorldAndHandlesEmptyLuaArrays(t *testing.T) {
	commander := &catalogCommander{receipt: dstruntime.CommandReceipt{OK: true, CompletedAt: time.Now(), Details: map[string]interface{}{"items": map[string]interface{}{}, "mods": map[string]interface{}{}, "total": 0, "offset": 0}}}
	result, err := SearchRuntime(context.Background(), commander, "remote-room", "caves", RuntimeSearchOptions{Limit: 60, Source: "mods"})
	if err != nil || len(result.Items) != 0 || result.Items == nil || commander.room != "remote-room" || commander.world != "caves" || commander.request.Action != "catalog.entities" {
		t.Fatalf("%#v %v %#v", result, err, commander)
	}
	commander.err = errors.New("Agent offline")
	if _, err = SearchRuntime(context.Background(), commander, "remote-room", "caves", RuntimeSearchOptions{Limit: 60}); !errors.Is(err, ErrRuntimeCatalogUnavailable) {
		t.Fatal(err)
	}
	if commander.calls != 2 {
		t.Fatal("unexpected fallback")
	}
	if _, err = SearchRuntime(context.Background(), commander, "room", "world", RuntimeSearchOptions{Limit: 121}); !errors.Is(err, ErrInvalidSearch) || commander.calls != 2 {
		t.Fatal("invalid query sent to runtime")
	}
}
func TestRuntimeSearchRetainsModProvenance(t *testing.T) {
	commander := &catalogCommander{receipt: dstruntime.CommandReceipt{OK: true, Details: map[string]interface{}{"items": []map[string]interface{}{{"id": "mod_sword", "nameZhCN": "模组剑", "modId": "workshop-123", "modName": "模组"}}, "mods": []interface{}{}, "total": 1, "offset": 0}}}
	result, err := SearchRuntime(context.Background(), commander, "r", "w", RuntimeSearchOptions{Limit: 60})
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if result.Items[0].ModID != "workshop-123" || result.Items[0].Key != "runtime:mod_sword" || result.Items[0].Type != "unknown" {
		t.Fatal(result.Items)
	}
}

func TestRuntimeSearchPreservesSnapshotAgeRefreshAndBusyError(t *testing.T) {
	observed := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	commander := &catalogCommander{receipt: dstruntime.CommandReceipt{OK: true, CompletedAt: observed.Add(time.Minute), Details: map[string]interface{}{"items": []interface{}{}, "mods": []interface{}{}, "total": 0, "offset": 0, "observedAt": observed.Format(time.RFC3339)}}}
	result, err := SearchRuntime(context.Background(), commander, "r", "w", RuntimeSearchOptions{Limit: 60, Refresh: true})
	if err != nil || !result.ObservedAt.Equal(observed) || commander.request.Arguments["refresh"] != true {
		t.Fatalf("result=%#v err=%v request=%#v", result, err, commander.request)
	}
	commander.err = dstruntime.ErrRuntimeCommandBusy
	if _, err = SearchRuntime(context.Background(), commander, "r", "w", RuntimeSearchOptions{Limit: 60}); !errors.Is(err, dstruntime.ErrRuntimeCommandBusy) {
		t.Fatalf("busy error lost: %v", err)
	}
}

func TestRuntimeCatalogCollectionLimitsRemainActionable(t *testing.T) {
	for _, code := range []string{"CATALOG_TOO_LARGE", "CATALOG_TIMEOUT"} {
		commander := &catalogCommander{receipt: dstruntime.CommandReceipt{OK: false, Code: code}}
		if _, err := SearchRuntime(context.Background(), commander, "r", "w", RuntimeSearchOptions{Limit: 60}); !errors.Is(err, ErrRuntimeCatalogLimit) {
			t.Fatalf("%s error: %v", code, err)
		}
	}
}
