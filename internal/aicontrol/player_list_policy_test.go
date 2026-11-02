package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"dont/internal/apicontract"
)

func TestPlayerListPolicyAcrossTypedAndDispatcher(t *testing.T) {
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, p Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		calls++
		policy, err := p.playerListAccess(in)
		if err != nil {
			t.Fatal(err)
		}
		return json.Marshal(map[string]any{"include": policy.Include})
	}))
	s.ConfigureAPI(catalog)
	for _, profile := range []string{"", ProfilePanelManage, ProfileGameManage, ProfileGameRead} {
		p := Principal{ID: "synthetic", Profile: profile, RoomIDs: []string{"allowed"}}
		admin := profile == "" || profile == ProfilePanelManage
		for _, query := range []string{"", `,"query":{}`, `,"query":{"includeAccessLists":false}`, `,"query":{"includeAccessLists":true}`} {
			for _, dispatch := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/dispatch=%t", profile, query, dispatch), func(t *testing.T) {
					before := calls
					raw := json.RawMessage(`{"path":{"roomId":"allowed"}` + query + `}`)
					denied := !admin && query == `,"query":{"includeAccessLists":true}`
					var result Invocation
					var err error
					if dispatch {
						result, err = s.dispatchMCP(context.Background(), p, append(append([]byte(`{"operationId":"listRoomPlayers","arguments":`), raw...), '}'))
					} else {
						result, err = s.Invoke(context.Background(), p, "api_listRoomPlayers", raw)
					}
					if denied {
						if !errors.Is(err, ErrDenied) || calls != before {
							t.Fatalf("unauthorized read: %v calls=%d", err, calls-before)
						}
						return
					}
					if err != nil || result.Status != "succeeded" || calls != before+1 {
						t.Fatalf("query failed/repeated: %+v %v", result, err)
					}
				})
			}
		}
		for _, raw := range []string{
			`{"path":{"roomId":"other"}}`,
			`{"path":{"roomId":"other"},"query":{"includeAccessLists":false}}`,
		} {
			before := calls
			if _, err := s.Invoke(context.Background(), p, "api_listRoomPlayers", []byte(raw)); !errors.Is(err, ErrDenied) || calls != before {
				t.Fatal("room scope bypass", err)
			}
		}
	}
}

func TestPlayerListRejectsAmbiguousAndSpoofedInputs(t *testing.T) {
	calls := 0
	s, _ := fixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{}`), nil
	}))
	catalog, _ := apicontract.Default()
	s.ConfigureAPI(catalog)
	p := Principal{ID: "synthetic", Profile: ProfileGameRead, RoomIDs: []string{"allowed"}}
	for _, raw := range []string{
		`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":null}}`,
		`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":"false"}}`,
		`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":0}}`,
		`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":[]}}`,
		`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":{}}}`,
		`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":true,"includeAccessLists":false}}`,
		`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":false,"includeAccessLists":false}}`,
		`{"path":{"roomId":"other","roomId":"allowed"}}`,
		`{"path":{"roomId":"allowed"},"query":{},"query":{}}`,
		`{"path":{"roomId":"allowed"},"profile":"panel-manage","readOnly":true}`,
		`{"path":{"roomId":"allowed"},"query":{"profile":"panel-manage"}}`,
	} {
		for _, dispatch := range []bool{false, true} {
			var err error
			if dispatch {
				_, err = s.dispatchMCP(context.Background(), p, []byte(`{"operationId":"listRoomPlayers","arguments":`+raw+`}`))
			} else {
				_, err = s.Invoke(context.Background(), p, "api_listRoomPlayers", []byte(raw))
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("dispatch=%t input=%s error=%v", dispatch, raw, err)
			}
		}
	}
	if _, err := s.dispatchMCP(context.Background(), p, []byte(`{"operationId":"listRoomPlayers","arguments":{},"arguments":{"path":{"roomId":"allowed"}}}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate dispatcher accepted", err)
	}
	if calls != 0 {
		t.Fatalf("invalid requests reached backend %d times", calls)
	}
}

func TestPlayerListDiscoveryDoesNotMutateSharedSchemas(t *testing.T) {
	s, _ := fixture(t, nil)
	catalog, _ := apicontract.Default()
	s.ConfigureAPI(catalog)
	op, _ := catalog.Operation("listRoomPlayers")
	shared, err := op.AIInputSchema()
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(shared)
	parameter := func(schema map[string]any) map[string]any {
		return schema["properties"].(map[string]any)["query"].(map[string]any)["properties"].(map[string]any)["includeAccessLists"].(map[string]any)
	}
	if _, hasDefault := parameter(shared)["default"]; hasDefault {
		t.Fatal("public contract has an identity-independent default")
	}
	var wg sync.WaitGroup
	for _, profile := range []string{"", ProfileGameRead, ProfilePanelManage, ProfileGameManage} {
		wg.Add(1)
		go func(profile string) {
			defer wg.Done()
			p := Principal{Profile: profile, RoomIDs: []string{"allowed"}}
			for range 12 {
				input, err := apiInputSchemaFor(p, op)
				if err != nil {
					t.Error(err)
					return
				}
				admin := profile == "" || profile == ProfilePanelManage
				prop := parameter(input)
				if prop["default"] != admin {
					t.Error("wrong projected default", profile, prop)
					return
				}
				if !admin && fmt.Sprint(prop["enum"]) != "[false]" {
					t.Error("read-only schema allows true", prop)
					return
				}
				data, err := s.discoverFor(p, json.RawMessage(`{"query":"listRoomPlayers","includeSchema":true}`))
				if err != nil {
					t.Error(err)
					return
				}
				var found struct {
					Items []struct {
						Input map[string]any `json:"inputSchema"`
						Hint  string         `json:"argumentHint"`
					} `json:"items"`
				}
				if json.Unmarshal(data, &found) != nil || len(found.Items) != 1 || found.Items[0].Hint == "" {
					t.Error("missing discovery", string(data))
					return
				}
				if parameter(found.Items[0].Input)["default"] != admin {
					t.Error("find_api policy differs", profile)
					return
				}
			}
		}(profile)
	}
	wg.Wait()
	after, _ := json.Marshal(shared)
	if string(before) != string(after) {
		t.Fatal("shared schema mutated")
	}
}

func TestDiscoverableReadOperationsAcceptMinimalAuthorizedInputs(t *testing.T) {
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{ProfileGameRead, ProfileGameManage} {
		p := Principal{Profile: profile, RoomIDs: []string{"allowed"}}
		for id, raw := range map[string]string{
			"listRooms":                `{}`,
			"listRoomPlayers":          `{"path":{"roomId":"allowed"}}`,
			"getRoomPlayerInventory":   `{"path":{"roomId":"allowed","playerId":"KU_EXAMPLE"}}`,
			"listRoomMods":             `{"path":{"roomId":"allowed"}}`,
			"getRoomPerformanceReport": `{"path":{"roomId":"allowed"}}`,
			"listWorldStates":          `{"path":{"roomId":"allowed"}}`,
			"queryRoomWorld":           `{"path":{"roomId":"allowed","worldId":"master","queryId":"world_info"}}`,
		} {
			op, ok := catalog.Operation(id)
			if !ok || !p.AllowsTool(op.ToolName()) {
				t.Fatal("missing allowed read", id)
			}
			in, err := op.Validate([]byte(raw))
			if err != nil {
				t.Fatal(id, err)
			}
			if err := p.CheckAPI(op, in); err != nil {
				t.Errorf("%s %s: %v", profile, id, err)
			}
		}
	}
}
