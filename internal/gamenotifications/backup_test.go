package gamenotifications

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBackupCountdownUsesRoomPolicyAndKeepsDeliveryHistory(t *testing.T) {
	for _, scenario := range []string{"online", "empty", "disabled", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			service, _, runtime, catalog := newNotificationTestService(t)
			counter := notificationTestOnline{count: 1}
			if scenario == "empty" {
				counter.count = 0
			}
			if scenario == "unknown" {
				// A missing online counter is unknown, never proof of an empty room.
				service.ConfigureOnlineCounter(nil)
			} else {
				service.ConfigureOnlineCounter(counter)
			}
			_, err := service.SavePolicy(catalog.room.ID, PolicyInput{Enabled: scenario != "disabled", CountdownSeconds: 10})
			if err != nil {
				t.Fatal(err)
			}
			waited := time.Duration(0)
			service.wait = func(_ context.Context, duration time.Duration) error { waited += duration; return nil }
			if err := service.BeforeOperation(context.Background(), catalog.room.ID, "backup", "", "backup-job"); err != nil {
				t.Fatal(err)
			}
			list, err := service.List(ListFilter{RoomID: catalog.room.ID})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "empty" || scenario == "disabled" {
				if list.Total != 0 || waited != 0 {
					t.Fatalf("policy ignored: %+v waited=%v", list, waited)
				}
			} else if list.Total != 1 || list.Items[0].Source != SourceBackup || list.Items[0].JobID != "backup-job" || waited != 10*time.Second || !strings.Contains(runtime.commands[catalog.worlds[0].ID], "停服备份，完成后自动恢复") {
				t.Fatalf("backup notice lost meaning or policy: %+v waited=%v", list, waited)
			}
		})
	}
}
