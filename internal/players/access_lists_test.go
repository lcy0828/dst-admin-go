package players

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBasePlayerListSkipsManagementSourcesForLocalAndRemote(t *testing.T) {
	for _, local := range []bool{true, false} {
		for _, scope := range []string{"current", "history"} {
			t.Run(fmt.Sprintf("local=%t/%s", local, scope), func(t *testing.T) {
				service, runtime, sender, access, _ := newPlayerTestService(t)
				runtime.local = &local
				if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
					t.Fatal(err)
				}
				if scope == "history" {
					if err := service.store.ObserveWorldSession("room", "master", "synthetic-session", service.now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
				}
				at := service.now()
				if err := service.store.SaveBan(Ban{RoomID: "room", PlayerID: "KU_ONE", Reason: "synthetic-management-detail", CreatedAt: at, ExpiresAt: &at}); err != nil {
					t.Fatal(err)
				}
				access.values.Blocked = []string{"KU_ONE"}
				filter := ListFilter{Scope: scope, Limit: 25}
				full, err := service.List("room", filter)
				if err != nil || !full.AccessListsAvailable || len(full.Items) != 1 || !full.Items[0].Banned || full.Items[0].BanReason == "" {
					t.Fatalf("admin behavior changed: %+v %v", full, err)
				}
				access.err = errors.New("synthetic offline management source")
				partial, err := service.List("room", filter)
				if err != nil || partial.AccessListsAvailable || len(partial.Warnings) == 0 || !partial.Items[0].Banned {
					t.Fatalf("source failure lost its meaning: %+v %v", partial, err)
				}
				// If the lightweight path touches controller ban storage, it must fail.
				if err := service.store.db.DropTable(service.store.bansTable).Error; err != nil {
					t.Fatal(err)
				}
				before, sent := access.readCount, len(sender.scripts)
				filter.SkipAccessLists = true
				base, err := service.List("room", filter)
				if err != nil || len(base.Items) != 1 {
					t.Fatalf("base query depends on management source: %+v %v", base, err)
				}
				if access.readCount != before || len(sender.scripts) != sent {
					t.Fatal("base query read Runtime config or sent a command")
				}
				item := base.Items[0]
				if base.AccessListsAvailable || base.Banned != 0 || item.Banned || item.BanReason != "" || item.BannedAt != nil || item.BanExpiresAt != nil || item.AccessListsAvailable || item.AccessWarning != "" || len(base.Warnings) != 0 {
					t.Fatalf("management data leaked: %+v", base)
				}
				encoded, _ := json.Marshal(base)
				if strings.Contains(string(encoded), "synthetic-management-detail") {
					t.Fatal("management detail leaked")
				}
			})
		}
	}
}
