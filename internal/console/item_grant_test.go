package console

import (
	"context"
	"dont/internal/dstruntime"
	"errors"
	"testing"
	"time"
)

func TestGrantItemDirectoryIsFixedAndInventoryOnly(t *testing.T) {
	for _, name := range []string{"肉丸", "meatballs", "Meatballs"} {
		item, err := ResolveGrantItem(name, 3)
		if err != nil || item.ID != "meatballs" {
			t.Fatal(name, item, err)
		}
	}
	for _, name := range []string{"beefalo", "spiderden", "mod_arbitrary", `meatballs\");c_shutdown()--`, "", " 肉丸"} {
		if _, err := ResolveGrantItem(name, 3); err == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, n := range []int{0, -1, 41, 1000} {
		if _, err := ResolveGrantItem("肉丸", n); err == nil {
			t.Fatal("accepted", n)
		}
	}
}

func TestItemGrantRequiresCorrelatedBusinessOutput(t *testing.T) {
	for _, tc := range []struct {
		name, output, status string
		runStatus            RunStatus
		truncated            bool
		granted              *int
	}{
		{"confirmed", "[DST-ADMIN-GIVE]\tKU_TEST123\tmeatballs\tunits\t3\n", "succeeded", RunSucceeded, false, ptrGrant(3)},
		{"no output", "", "uncertain", RunSucceeded, false, nil},
		{"sent", "[DST-ADMIN-GIVE]\tKU_TEST123\tmeatballs\tunits\t3\n", "uncertain", RunSent, false, nil},
		{"wrong player", "[DST-ADMIN-GIVE]\tKU_OTHER123\tmeatballs\tunits\t3\n", "uncertain", RunSucceeded, false, nil},
		{"wrong count", "[DST-ADMIN-GIVE]\tKU_TEST123\tmeatballs\tunits\t2\n", "uncertain", RunSucceeded, false, nil},
		{"truncated", "[DST-ADMIN-GIVE]\tKU_TEST123\tmeatballs\tunits\t3\n", "uncertain", RunSucceeded, true, nil},
		{"duplicate", "[DST-ADMIN-GIVE]\tKU_TEST123\tmeatballs\tunits\t3\n[DST-ADMIN-GIVE]\tKU_TEST123\tmeatballs\tunits\t3\n", "uncertain", RunSucceeded, false, nil},
		{"migrated", "[DST-ADMIN-GIVE]\tPLAYER_NOT_FOUND\tKU_TEST123\n", "failed", RunSucceeded, false, ptrGrant(0)},
		{"partial lua error", "", "failed", RunFailed, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := Run{ID: "run", RoomID: "room", WorldID: "caves", Mode: "builtin", CommandID: "give_item", Arguments: map[string]interface{}{"player_id": "KU_TEST123", "prefab": "meatballs", "count": float64(3), "quantity_mode": "units"}, Status: tc.runStatus, ExecutionOutcome: "confirmed", Output: &Output{Text: tc.output, Truncated: tc.truncated}}
			r := projectItemGrant(run)
			if r.Status != tc.status || (r.Granted == nil) != (tc.granted == nil) || r.Granted != nil && *r.Granted != *tc.granted {
				t.Fatalf("%+v", r)
			}
			if tc.status == "uncertain" && (r.Complete || !r.MayHaveExecuted) {
				t.Fatal(r)
			}
		})
	}
}
func ptrGrant(n int) *int { return &n }

type grantLateReceipt struct{ sends, reads int }

func (g *grantLateReceipt) ExecuteCommand(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	g.sends++
	return dstruntime.CommandReceipt{}, errors.Join(dstruntime.ErrRuntimeCommandUnconfirmed, dstruntime.ErrRuntimeResultAbsent)
}
func (g *grantLateReceipt) ReadCommandReceipt(_ context.Context, _, _ string, request dstruntime.CommandRequest, _ time.Time) (dstruntime.CommandReceipt, error) {
	g.reads++
	return dstruntime.CommandReceipt{RequestID: request.RequestID, Action: request.Action, OK: true, CompletedAt: time.Now(), Details: map[string]interface{}{"output": map[string]interface{}{"text": "[DST-ADMIN-GIVE]\tKU_TEST123\tmeatballs\tunits\t3\n", "truncated": false}}}, nil
}
func TestItemGrantLateReceiptNeverReissuesMutation(t *testing.T) {
	s := newConsoleService(t, &captureSender{})
	commander := &grantLateReceipt{}
	s.commander = commander
	r, err := s.GiveItem(context.Background(), "room", "caves", "KU_TEST123", "肉丸", 3)
	if err != nil || r.Status != "uncertain" || r.Complete {
		t.Fatal(r, err)
	}
	r, err = s.ItemGrant(context.Background(), "room", "KU_TEST123", r.RunID, true)
	if err != nil || r.Status != "succeeded" || r.Granted == nil || *r.Granted != 3 || commander.sends != 1 || commander.reads != 1 {
		t.Fatal(r, err, commander)
	}
	_, err = s.ItemGrant(context.Background(), "other-room", "KU_TEST123", r.RunID, true)
	if !errors.Is(err, ErrRunNotFound) || commander.reads != 1 {
		t.Fatal(err, commander)
	}
}
