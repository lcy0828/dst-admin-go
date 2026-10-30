package dstruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/shared"
	lua "github.com/yuin/gopher-lua"
)

func queryLuaState(tb testing.TB, count int) (*lua.LState, *string) {
	tb.Helper()
	L := lua.NewState()
	preloadGameJSONHarness(L)
	var output string
	L.SetGlobal("print", L.NewFunction(func(L *lua.LState) int { output = L.CheckString(1); return 0 }))
	entities := L.NewTable()
	for i := 1; i <= count; i++ {
		e := L.NewTable()
		e.RawSetString("prefab", lua.LString("spiderden"))
		entities.RawSetInt(i, e)
	}
	L.SetGlobal("Ents", entities)
	if err := L.DoString(`
local function forbidden() error("QUERY_MUTATED_GAME") end
SpawnPrefab=forbidden;c_save=forbidden;c_announce=forbidden;c_regenerateworld=forbidden
TheWorld={state={cycles=7,season="autumn",phase="day",temperature=20},PushEvent=forbidden,DoTaskInTime=forbidden,DoPeriodicTask=forbidden}
AllPlayers={{userid="KU_EXAMPLE",prefab="wilson"}}
TheNet={Announce=forbidden,SendRemoteExecute=forbidden,GetClientTable=function() return {
 {userid="KU_HOST",name="[Host]"},
 {userid="KU_EXAMPLE",name="Example",prefab="wilson",netid="private-not-returned"},
 {userid="KU_OTHER_SHARD",name="Other",prefab="willow"},
 {userid="KU_EXAMPLE",name="Example",prefab="wilson"}
} end}
scheduler={ExecuteInTime=forbidden};staticScheduler=scheduler
`); err != nil {
		tb.Fatal(err)
	}
	return L, &output
}
func queryScript(tb testing.TB, id, prefab string) string {
	tb.Helper()
	r, err := worldQueryCommand(WorldQueryRequest{RequestID: "query-test-request-1234", QueryID: id, Prefab: prefab, ExpectedIdentity: "example-instance"})
	if err != nil {
		tb.Fatal(err)
	}
	return r.Arguments["script"].(string)
}
func TestFixedQueriesAreReadOnlyAndProjectOnlyApprovedFields(t *testing.T) {
	for _, id := range []string{"entity_count", "world_info", "online_players"} {
		t.Run(id, func(t *testing.T) {
			L, output := queryLuaState(t, 20)
			defer L.Close()
			if err := L.DoString(`os.clock=function()return 0 end`); err != nil {
				t.Fatal(err)
			}
			prefab := ""
			if id == "entity_count" {
				prefab = "spiderden"
			}
			if err := L.DoString(queryScript(t, id, prefab)); err != nil {
				t.Fatal(err)
			}
			var v struct {
				Complete bool           `json:"complete"`
				Data     map[string]any `json:"data"`
			}
			if json.Unmarshal([]byte(*output), &v) != nil || !v.Complete {
				t.Fatal(*output)
			}
			switch id {
			case "entity_count":
				if v.Data["count"] != float64(20) {
					t.Fatal(v)
				}
			case "online_players":
				if v.Data["count"] != float64(1) || strings.Contains(*output, "private-not-returned") || strings.Contains(*output, "KU_HOST") || strings.Contains(*output, "KU_OTHER_SHARD") {
					t.Fatal(*output)
				}
			case "world_info":
				if v.Data["cycles"] != float64(7) {
					t.Fatal(v)
				}
			}
		})
	}
}
func TestQueryTemplateRejectsUnknownDefinitionsAndInjectedParameters(t *testing.T) {
	for _, q := range []WorldQueryRequest{
		{QueryID: "save_world"}, {QueryID: "c_announce"}, {QueryID: "../commands"}, {QueryID: "entity_count", Prefab: `spiderden");SpawnPrefab("beefalo")--`},
		{QueryID: "world_info", Prefab: "spiderden"}, {QueryID: "entity_count", Prefab: strings.Repeat("a", 81)},
	} {
		q.RequestID = "query-test-request-1234"
		q.ExpectedIdentity = "instance"
		if _, err := worldQueryCommand(q); !errors.Is(err, ErrRuntimeRequestInvalid) {
			t.Fatal(q, err)
		}
	}
	if err := ValidateWorldQuery("entity_count", "ModPrefab_42"); err != nil {
		t.Fatal("mod prefab rejected", err)
	}
}
func TestQueryScanHasHardAndCooperativeBudgets(t *testing.T) {
	for _, tc := range []struct {
		name, clock, reason string
		count, scanned      int
	}{
		{"hard cap", `os.clock=function()return 0 end`, "SCAN_LIMIT", 100001, 100000},
		{"CPU budget", `local n=0;os.clock=function()n=n+1;if n>2 then return 1 else return 0 end end`, "CPU_BUDGET", 1000, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			L, out := queryLuaState(t, tc.count)
			defer L.Close()
			if err := L.DoString(tc.clock); err != nil {
				t.Fatal(err)
			}
			if err := L.DoString(queryScript(t, "entity_count", "spiderden")); err != nil {
				t.Fatal(err)
			}
			var v struct {
				Complete        bool
				Reason          string
				ScannedEntities int
				Data            struct {
					Count      *int
					LowerBound int
				}
			}
			if json.Unmarshal([]byte(*out), &v) != nil || v.Complete || v.Reason != tc.reason || v.ScannedEntities != tc.scanned || v.Data.Count != nil || v.Data.LowerBound != tc.scanned {
				t.Fatal(*out)
			}
		})
	}
}
func TestDistributedQueryDoesNotRefreshOrDispatchWhenStaleBusyOrChanged(t *testing.T) {
	for _, test := range []string{"stale", "busy", "identity", "offline", "lock"} {
		t.Run(test, func(t *testing.T) {
			b, f, room, world, now := newDistributedBridgeFixture(t)
			identity, err := b.QueryIdentity(context.Background(), room, world)
			if err != nil {
				t.Fatal(err)
			}
			var h Health
			json.Unmarshal(f.bundles[shared.ArtifactRuntimeHealth].Artifacts[0].Data, &h)
			switch test {
			case "stale":
				old := now.Add(-time.Hour).Unix()
				h.HeartbeatAtUnix = &old
				h.HeartbeatSequence = 1
				h.LastCapturedAtUnix = &old
				h.LastWrittenAtUnix = &old
			case "busy":
				m := h.Modules["commands"]
				m.Busy = true
				h.Modules["commands"] = m
			case "identity":
				identity = "wrong-instance"
			case "offline":
				f.status.State = "stopped"
			case "lock":
				b.worldLock(room, world).Lock()
				defer b.worldLock(room, world).Unlock()
			}
			f.bundles[shared.ArtifactRuntimeHealth] = artifactBundle(shared.ArtifactRuntimeHealth, now, "health.json", h)
			_, err = b.ExecuteWorldQuery(context.Background(), room, world, WorldQueryRequest{RequestID: "query-test-request-1234", QueryID: "entity_count", Prefab: "spiderden", ExpectedIdentity: identity})
			if err == nil || len(f.sent) != 0 {
				t.Fatal(test, err, len(f.sent))
			}
		})
	}
}
func TestDistributedQueryUsesExactReceiptAndNeverReplays(t *testing.T) {
	b, f, room, world, now := newDistributedBridgeFixture(t)
	identity, err := b.QueryIdentity(context.Background(), room, world)
	if err != nil {
		t.Fatal(err)
	}
	q := WorldQueryRequest{RequestID: "query-test-request-1234", QueryID: "entity_count", Prefab: "spiderden", ExpectedIdentity: identity}
	f.onSend = func(send shared.RuntimeConsoleRequest) {
		if strings.Contains(send.Command, "Refresh") {
			t.Error("query sent refresh")
		}
		r := CommandReceipt{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 1, RequestID: q.RequestID, Action: "console.execute", OK: true, Code: "COMMAND_EXECUTED", CompletedAtUnix: now.Unix()}
		f.mu.Lock()
		f.bundles[shared.ArtifactRuntimeCommand] = artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", r)
		f.mu.Unlock()
	}
	r, err := b.ExecuteWorldQuery(context.Background(), room, world, q)
	if err != nil || !r.OK || len(f.sent) != 1 {
		t.Fatal(r, err, len(f.sent))
	}
	if f.sent[0].CommandDocument == nil {
		t.Fatal("expected bounded command document delivery")
	}
}
func BenchmarkWorldQueryEntityScan(b *testing.B) {
	for _, n := range []int{10000, 50000, 100000, 100001} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			L, out := queryLuaState(b, n)
			defer L.Close()
			script := queryScript(b, "entity_count", "spiderden")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := L.DoString(script); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			var v struct {
				Scanned int `json:"scannedEntities"`
			}
			json.Unmarshal([]byte(*out), &v)
			b.ReportMetric(float64(v.Scanned), "last-scanned")
		})
	}
}

func TestLocalQueryReusesReceiptChannelWithoutRefresh(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Now().UTC().Truncate(time.Second)
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	for _, name := range []string{"save/session/SESSION", "save/mod_config_data/dst-admin"} {
		if err := os.MkdirAll(filepath.Join(worldPath, name), 0750); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{"server.ini": "[SHARD]\nid = 1\n", "server_log.txt": "[00:00:00]: Current time: " + now.Add(-time.Minute).In(time.Local).Format(time.ANSIC) + "\n"} {
		if err := os.WriteFile(filepath.Join(worldPath, name), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	at := now.Unix()
	h := Health{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1", Running: true, Ready: true, LastCapturedAtUnix: &at, LastWrittenAtUnix: &at, Modules: map[string]ModuleHealth{"commands": {Running: true, Ready: true, ProducerInstanceID: "instance"}}}
	writeJSONFile(t, filepath.Join(worldPath, "save/mod_config_data/dst-admin/health.json"), h)
	sender := &bridgeSender{root: root, now: now.Add(time.Second), reply: true}
	b, err := NewBridge(manager, bridgeProcess{running: true}, sender)
	if err != nil {
		t.Fatal(err)
	}
	b.now = func() time.Time { return now.Add(time.Second) }
	b.timeout = 200 * time.Millisecond
	r, err := b.ExecuteWorldQuery(context.Background(), catalog.room.ID, catalog.worlds[0].ID, WorldQueryRequest{RequestID: "local-query-request-1234", QueryID: "entity_count", Prefab: "spiderden", ExpectedIdentity: queryIdentity(h)})
	if err != nil || !r.OK || len(sender.scripts) != 1 || strings.Contains(sender.scripts[0], "Refresh") {
		t.Fatal(r, err, sender.scripts)
	}
}

func TestPlayerQueryFallsBackToLocalEntitiesAndBoundsOutput(t *testing.T) {
	L, out := queryLuaState(t, 0)
	defer L.Close()
	if err := L.DoString(`os.clock=function()return 0 end;TheNet.GetClientTable=function()return {} end`); err != nil {
		t.Fatal(err)
	}
	if err := L.DoString(queryScript(t, "online_players", "")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*out, `"count":1`) || !strings.Contains(*out, "KU_EXAMPLE") {
		t.Fatal("local player lost when metadata lags", *out)
	}
	if err := L.DoString(`AllPlayers={};local clients={};for i=1,64 do local id="KU_EXAMPLE_"..i;AllPlayers[i]={userid=id,prefab="wilson"};clients[i]={userid=id,name=string.rep("玩家",40),prefab="wilson"} end;TheNet.GetClientTable=function()return clients end`); err != nil {
		t.Fatal(err)
	}
	if err := L.DoString(queryScript(t, "online_players", "")); err != nil {
		t.Fatal(err)
	}
	if len(*out) > 8192 || !strings.Contains(*out, `"complete":false`) || !strings.Contains(*out, `"reason":"OUTPUT_LIMIT"`) {
		t.Fatal("output budget missing", *out)
	}
}
