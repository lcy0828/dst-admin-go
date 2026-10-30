package aicontrol

import (
	"context"
	"dont/internal/apicontract"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func botFixture(t *testing.T, backend apiBackendFunc) (*Service, *Store) {
	t.Helper()
	s, store := fixture(t, backend)
	catalog, e := apicontract.Default()
	if e != nil {
		t.Fatal(e)
	}
	s.ConfigureAPI(catalog)
	return s, store
}
func botInput(t *testing.T, s *Service, p Principal, id, request string) BotInput {
	t.Helper()
	c, e := s.botCapability(p, id, "room-a")
	if e != nil {
		t.Fatal(e)
	}
	return BotInput{RoomID: "room-a", CapabilityID: id, ContractVersion: 1, SchemaRevision: c.SchemaRevision, RequestID: request, Parameters: map[string]any{}}
}
func TestBotDirectoryPermissionsAndAtomicPages(t *testing.T) {
	s, _ := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		t.Fatal("discovery executed backend")
		return nil, nil
	}))
	p := Principal{ID: "reader", Profile: ProfileGameRead, RoomIDs: []string{"room-a"}}
	page, e := s.BotDirectory(p, 1, 2, "", "")
	if e != nil {
		t.Fatal(e)
	}
	revision := page["revision"].(string)
	cursor := page["nextCursor"].(string)
	seen := map[string]bool{}
	for {
		for _, c := range page["items"].([]BotCapability) {
			if c.Access != "read" || seen[c.ID] {
				t.Fatal(c)
			}
			seen[c.ID] = true
		}
		if cursor == "" {
			break
		}
		page, e = s.BotDirectory(p, 1, 2, "", cursor)
		if e != nil {
			t.Fatal(e)
		}
		cursor = page["nextCursor"].(string)
	}
	v, e := s.BotDirectory(p, 1, 2, revision, "")
	if e != nil || v["notModified"] != true {
		t.Fatal(v, e)
	}
	if _, e = s.BotDirectory(Principal{ID: "other", Profile: ProfileGameRead, RoomIDs: []string{"room-a"}}, 1, 2, "", pageCursor(t, s, p)); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if !seen["room.status"] || seen["room.backup.create"] || seen["room.backups.settings"] {
		t.Fatal(seen)
	}
}
func pageCursor(t *testing.T, s *Service, p Principal) string {
	t.Helper()
	page, e := s.BotDirectory(p, 1, 2, "", "")
	if e != nil {
		t.Fatal(e)
	}
	return page["nextCursor"].(string)
}
func TestBotWriteLostReplyRecoveryAndRestartNeverReplay(t *testing.T) {
	var executions atomic.Int32
	s, store := botFixture(t, apiBackendFunc(func(_ context.Context, p Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		if op.ID == "getJob" {
			return json.RawMessage(`{"httpStatus":200,"data":{"id":"job-a","roomId":"room-a","status":"succeeded","outcome":"full"}}`), nil
		}
		executions.Add(1)
		if op.ID != "createRoomBackup" || len(in.RequestID) > 128 {
			t.Fatal(op.ID, in)
		}
		return json.RawMessage(`{"httpStatus":202,"data":{"id":"job-a","roomId":"room-a","status":"queued"}}`), nil
	}))
	p := Principal{ID: "manager", Profile: ProfileGameManage, RoomIDs: []string{"room-a"}}
	in := botInput(t, s, p, "room.backup.create", strings.Repeat("a", 128))
	first, e := s.BotExecute(context.Background(), p, in, true)
	if e != nil || first.Complete || first.Status != "pending" {
		t.Fatal(first, e)
	}
	again, e := s.BotExecute(context.Background(), p, in, true)
	if e != nil || again.JobID != first.JobID || executions.Load() != 1 {
		t.Fatal(again, e, executions.Load())
	}
	if e = store.Migrate(); e != nil {
		t.Fatal(e)
	}
	result, e := s.BotResult(context.Background(), p, "room-a", in.RequestID)
	if e != nil || !result.Complete || result.Status != "succeeded" || executions.Load() != 1 {
		t.Fatal(result, e)
	}
	if _, e = s.BotResult(context.Background(), Principal{ID: "other", Profile: p.Profile, RoomIDs: p.RoomIDs}, "room-a", in.RequestID); e == nil {
		t.Fatal("cross owner result")
	}
	changed := in
	changed.Parameters = map[string]any{"readOnly": true}
	if _, e = s.BotExecute(context.Background(), p, changed, true); e == nil {
		t.Fatal("forged parameter")
	}
}
func TestBotValidationAndUnknownMetadataCannotGrantWrites(t *testing.T) {
	var executions atomic.Int32
	s, _ := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		executions.Add(1)
		return json.RawMessage(`{"httpStatus":200,"data":{"items":[],"online":0}}`), nil
	}))
	p := Principal{ID: "reader", Profile: ProfileGameRead, RoomIDs: []string{"room-a"}}
	in := botInput(t, s, p, "room.players", "query-request-1")
	r, e := s.BotExecute(context.Background(), p, in, false)
	if e != nil || !r.Complete {
		t.Fatal(r, e)
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `{"lua":"x"} {}`, `{"x":NaN}`, `{"x":{"x":{"x":{"x":{"x":1}}}}}`} {
		if _, e = StrictBotJSON([]byte(raw)); e == nil {
			t.Fatal(raw)
		}
	}
	for _, modify := range []func(*BotInput){func(v *BotInput) { v.CapabilityID = "custom.command" }, func(v *BotInput) { v.Parameters = map[string]any{"risk": "low", "readOnly": true} }, func(v *BotInput) { v.RoomID = "other" }, func(v *BotInput) { v.SchemaRevision = "wrong" }} {
		bad := in
		modify(&bad)
		if _, e = s.BotExecute(context.Background(), p, bad, false); e == nil {
			t.Fatal(bad)
		}
	}
	in.CapabilityID = "room.backup.create"
	if _, e = s.BotExecute(context.Background(), p, in, true); e == nil {
		t.Fatal("read identity wrote")
	}
	if executions.Load() != 1 {
		t.Fatal(executions.Load())
	}
}
func TestBotUncertainPersistedAndInterruptedRequestProtected(t *testing.T) {
	var executions atomic.Int32
	s, store := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		executions.Add(1)
		return nil, context.DeadlineExceeded
	}))
	p := principal()
	in := botInput(t, s, p, "room.restart", "uncertain-request-1")
	r, e := s.BotExecute(context.Background(), p, in, true)
	if e != nil || r.Status != "uncertain" || !r.MayHaveExecuted {
		t.Fatal(r, e)
	}
	if e = store.Migrate(); e != nil {
		t.Fatal(e)
	}
	r, e = s.BotResult(context.Background(), p, "room-a", in.RequestID)
	if e != nil || r.Status != "uncertain" {
		t.Fatal(r, e)
	}
	if _, e = s.BotExecute(context.Background(), p, in, true); e != nil || executions.Load() != 1 {
		t.Fatal(e, executions.Load())
	}
	// Simulate process loss before the receipt association, as Store.Migrate does.
	store.db.Table(store.callTable).Where("tool = ?", "bot.write").Updates(map[string]any{"status": "executing", "result": ""})
	if e = store.Migrate(); e != nil {
		t.Fatal(e)
	}
	r, e = s.BotResult(context.Background(), p, "room-a", in.RequestID)
	if e != nil || r.Status != "uncertain" || r.Complete {
		t.Fatal(r, e)
	}
}

func TestBotStrictMCPInputAndReadEndpointCannotSubmitWrite(t *testing.T) {
	executions := 0
	s, _ := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		executions++
		return json.RawMessage(`{"httpStatus":200,"data":{}}`), nil
	}))
	in := botInput(t, s, principal(), "room.backup.create", "query-is-not-write")
	if _, e := s.BotExecute(context.Background(), principal(), in, false); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
	in = botInput(t, s, principal(), "world.entity_count", "injection-not-allowed")
	in.Parameters = map[string]any{"worldId": "world-a", "prefab": "spiderden\");SpawnPrefab(\"beefalo\")"}
	if _, e := s.BotExecute(context.Background(), principal(), in, false); e == nil {
		t.Fatal("Lua injection accepted")
	}
	raw := `{"path":{"roomId":"room-a","capabilityId":"room.backup.create"},"body":{"contractVersion":1,"schemaRevision":"` + botInput(t, s, principal(), "room.backup.create", "duplicate-key-1").SchemaRevision + `","parameters":{"enabled":false,"enabled":true}},"requestId":"duplicate-key-1"}`
	if _, e := s.Invoke(context.Background(), principal(), "api_runBotCapability", []byte(raw)); e == nil {
		t.Fatal("MCP duplicate field accepted")
	}
	if executions != 0 {
		t.Fatal("invalid inputs reached game", executions)
	}
}
func TestBotSynchronousScheduleIsConfirmedWithoutClaimingBackup(t *testing.T) {
	s, _ := botFixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		if op.ID != "configureRoomBackupSchedule" || in.Body.(map[string]any)["enabled"] != true {
			t.Fatal(op.ID, in)
		}
		return json.RawMessage(`{"httpStatus":200,"data":{"effectiveEnabled":true,"source":"automation","nextRunLocal":"example-next-run"}}`), nil
	}))
	in := botInput(t, s, principal(), "room.backups.configure", "schedule-update-1")
	in.Parameters = map[string]any{"enabled": true}
	result, e := s.BotExecute(context.Background(), principal(), in, true)
	if e != nil || !result.Complete || result.ExecutionOutcome != "confirmed" || result.JobID != "" || result.ResultKind != "facts" {
		t.Fatal(result, e)
	}
}
func TestBotCompactionKeepsRequestIdentityAndLimitsBeforeDispatch(t *testing.T) {
	executions := 0
	s, store := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		executions++
		return json.RawMessage(`{"httpStatus":200,"data":{"id":"job-a","status":"succeeded","outcome":"full"}}`), nil
	}))
	in := botInput(t, s, principal(), "room.backup.create", "old-completed-request")
	if _, e := s.BotExecute(context.Background(), principal(), in, true); e != nil {
		t.Fatal(e)
	}
	if e := store.db.Table(store.callTable).Where("tool = ?", "bot.write").Update("created_at", time.Now().UTC().Add(-8*24*time.Hour)).Error; e != nil {
		t.Fatal(e)
	}
	if e := store.compactBotRecords(); e != nil {
		t.Fatal(e)
	}
	r, e := s.BotExecute(context.Background(), principal(), in, true)
	if e != nil || r.Status != "unavailable" || r.Error["code"] != "BOT_RESULT_EXPIRED" || executions != 1 {
		t.Fatal(r, e, executions)
	}
	// All shared tool slots occupied: reject before reserving or invoking a write.
	s.toolSlots <- struct{}{}
	s.toolSlots <- struct{}{}
	in.RequestID = "new-busy-request"
	if _, e = s.BotExecute(context.Background(), principal(), in, true); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	<-s.toolSlots
	<-s.toolSlots
	var count int
	store.db.Table(store.callTable).Where("tool = ?", "bot.write").Count(&count)
	if count != 1 {
		t.Fatal("busy request reserved", count)
	}
}

func TestBotSchemaRevisionSeparatesPresentationFromAuthorization(t *testing.T) {
	capability := BotCapabilities()[0]
	original := botSchemaRevision(capability)
	capability.Title = "a renamed menu"
	if botSchemaRevision(capability) != original {
		t.Fatal("title invalidated form")
	}
	capability.Category = "a-new-private-category"
	if botSchemaRevision(capability) == original {
		t.Fatal("privacy change retained old executable contract")
	}
}
func TestBotUnderlyingIdempotencySeparatesCredentials(t *testing.T) {
	requests := []string{}
	s, _ := botFixture(t, apiBackendFunc(func(_ context.Context, _ Principal, _ *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		requests = append(requests, in.RequestID)
		return json.RawMessage(`{"httpStatus":202,"data":{"id":"job","status":"queued"}}`), nil
	}))
	first := principal()
	second := first
	second.ID = "another-credential"
	for _, p := range []Principal{first, second} {
		in := botInput(t, s, p, "room.backup.create", "same-request-different-identities")
		if _, e := s.BotExecute(context.Background(), p, in, true); e != nil {
			t.Fatal(e)
		}
	}
	if len(requests) != 2 || requests[0] == requests[1] {
		t.Fatal("underlying HTTP cache merged distinct credentials", requests)
	}
}

func TestBotPublicPlayerProjectionSeparatesNetworkData(t *testing.T) {
	s, _ := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		return json.RawMessage(`{"httpStatus":200,"data":{"online":1,"items":[{"id":"KU_example","name":"example","online":true,"netScore":0,"performance":0,"health":100,"fields":{"netScore":{"status":"live"},"health":{"status":"live"}}}]}}`), nil
	}))
	p := principal()
	plain, e := s.BotExecute(context.Background(), p, botInput(t, s, p, "room.players", "public-players-001"), false)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(plain.Data)
	if strings.Contains(string(data), "netScore") || strings.Contains(string(data), "health") {
		t.Fatal("player category exposed unrelated fields", string(data))
	}
	network, e := s.BotExecute(context.Background(), p, botInput(t, s, p, "room.network", "network-players-001"), false)
	if e != nil {
		t.Fatal(e)
	}
	data, _ = json.Marshal(network.Data)
	if !strings.Contains(string(data), `"netScore":0`) || strings.Contains(string(data), "health") {
		t.Fatal("network projection lost a valid zero or exposed vitals", string(data))
	}
}
