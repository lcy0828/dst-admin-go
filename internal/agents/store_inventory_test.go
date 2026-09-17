package agents

import (
	"encoding/json"
	"testing"
	"time"

	"dont/shared"
)

func TestInventoryStoreUpgradesLegacyPayloadAndKeepsInstallationsIndependent(t *testing.T) {
	_, store, _, _ := newAgentTestService(t)
	now := time.Now().UTC().Truncate(time.Second)
	primary := shared.RuntimeInventoryReport{
		ProtocolVersion: shared.RuntimeInventoryProtocolVersion,
		ObservedAt:      now,
		Installation:    shared.RuntimeInstallationReport{ID: "primary", SavePath: "/srv/primary", ServerPath: "/opt/primary"},
		Rooms:           []shared.RoomInventoryReport{{Directory: "Cluster_A"}},
		Processes: []shared.ShardProcessReport{
			{PID: 100, Executable: "tmux: server", Cluster: "Cluster_A", Shard: "Master"},
			{PID: 101, Executable: "/opt/primary/bin64/dontstarve_dedicated_server_nullrenderer_x64", Cluster: "Cluster_A", Shard: "Master"},
		},
	}
	payload, err := json.Marshal(primary)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Table(store.inventoryTable).Create(&inventoryRecord{
		AgentID: "agent-primary", Payload: string(payload), ObservedAt: now, ReceivedAt: now,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	legacy, err := store.InventoryForInstallation("agent-primary", "primary")
	if err != nil || legacy.InstallationID != "primary" || legacy.Inventory.Installation.SavePath != "/srv/primary" {
		t.Fatalf("legacy inventory=%#v err=%v", legacy, err)
	}
	if len(legacy.Inventory.Processes) != 1 || legacy.Inventory.Processes[0].PID != 101 {
		t.Fatalf("legacy supervisor process was not filtered: %#v", legacy.Inventory.Processes)
	}

	secondary := shared.RuntimeInventoryReport{
		ProtocolVersion: shared.RuntimeInventoryProtocolVersion,
		ObservedAt:      now.Add(time.Second),
		Installation:    shared.RuntimeInstallationReport{ID: "testing", SavePath: "/srv/testing", ServerPath: "/opt/testing"},
		Rooms:           []shared.RoomInventoryReport{{Directory: "Cluster_B"}},
	}
	if _, err := store.SaveInventory("agent-primary", secondary); err != nil {
		t.Fatal(err)
	}
	items, err := store.Inventories("agent-primary")
	if err != nil || len(items) != 2 || items[0].InstallationID != "primary" || items[1].InstallationID != "testing" {
		t.Fatalf("inventories=%#v err=%v", items, err)
	}
	if items[0].Inventory.Rooms[0].Directory != "Cluster_A" || items[1].Inventory.Rooms[0].Directory != "Cluster_B" {
		t.Fatalf("installation payloads were overwritten: %#v", items)
	}
	var record inventoryRecord
	if err := store.db.Table(store.inventoryTable).Where("agent_id = ?", "agent-primary").First(&record).Error; err != nil {
		t.Fatal(err)
	}
	bundle := storedInventoryBundle{}
	if err := json.Unmarshal([]byte(record.Payload), &bundle); err != nil || bundle.Version != inventoryBundleVersion || len(bundle.Items) != 2 {
		t.Fatalf("stored bundle=%#v err=%v", bundle, err)
	}
}
