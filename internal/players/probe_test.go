package players

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dont/internal/rooms"
)

func TestParseProbeOutputDecodesIdentityAndVitals(t *testing.T) {
	nonce := "probe-1"
	output := "[12:00:00] [DST-ADMIN-PLAYERS probe-1 ITEM] " + strings.Join([]string{
		"KU_ONE", "Willow%20The%20Brave", "willow", "42", "1", "7656119", "1", "92.5", "61", "74", "31.2", "8",
	}, "\t") + "\n[12:00:00] [DST-ADMIN-PLAYERS probe-1 DONE]\n"
	items, complete, err := parseProbeOutput(output, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || len(items) != 1 || items[0].Name != "Willow The Brave" || items[0].NetScore == nil || *items[0].NetScore != 1 || items[0].HealthPercent == nil || *items[0].HealthPercent != 92.5 {
		t.Fatalf("unexpected probe result: complete=%v items=%#v", complete, items)
	}
}

func TestParseProbeOutputIgnoresDedicatedHostWithPlayerIdentity(t *testing.T) {
	nonce := "probe-host"
	lines := []string{
		"[DST-ADMIN-PLAYERS probe-host ITEM] " + strings.Join([]string{
			"KU_ONE", "%5BHost%5D", "", "0", "1", "", "-1", "-1", "-1", "-1", "-999", "-1",
		}, "\t"),
		"[DST-ADMIN-PLAYERS probe-host ITEM] " + strings.Join([]string{
			"KU_ONE", "Willow", "willow", "42", "1", "7656119", "0", "92.5", "61", "74", "31.2", "8",
		}, "\t"),
		"[DST-ADMIN-PLAYERS probe-host DONE]",
	}
	items, complete, err := parseProbeOutput(strings.Join(lines, "\n"), nonce)
	if err != nil || !complete || len(items) != 1 || items[0].Name != "Willow" {
		t.Fatalf("dedicated host was not ignored: complete=%v items=%#v err=%v", complete, items, err)
	}
}

func TestParseProbeHistoryUsesLatestCompletedNonEmptySnapshot(t *testing.T) {
	lines := []string{
		"[DST-ADMIN-PLAYERS first ITEM] " + strings.Join([]string{
			"KU_OLD", "Wilson", "wilson", "3", "0", "7656111", "5", "80", "70", "60", "20", "0",
		}, "\t"),
		"[DST-ADMIN-PLAYERS first DONE]",
		"[DST-ADMIN-PLAYERS latest ITEM] " + strings.Join([]string{
			"KU_ONE", "%5BHost%5D", "", "0", "1", "", "0", "-1", "-1", "-1", "-999", "-1",
		}, "\t"),
		"[DST-ADMIN-PLAYERS latest ITEM] " + strings.Join([]string{
			"KU_ONE", "lcy", "wendy", "9", "1", "7656119", "4", "90", "80", "70", "21", "1",
		}, "\t"),
		"[DST-ADMIN-PLAYERS latest ITEM] " + strings.Join([]string{
			"KU_ONE", "lcy", "wendy", "9", "1", "7656119", "4", "90", "80", "70", "21", "1",
		}, "\t"),
		"[DST-ADMIN-PLAYERS latest DONE]",
		"[DST-ADMIN-PLAYERS empty DONE]",
		"[DST-ADMIN-PLAYERS incomplete ITEM] " + strings.Join([]string{
			"KU_NEW", "Wanda", "wanda", "1", "0", "7656222", "0", "90", "80", "70", "21", "1",
		}, "\t"),
	}
	items := parseProbeHistory(strings.Join(lines, "\n"))
	if len(items) != 1 || items[0].ID != "KU_ONE" || items[0].Name != "lcy" {
		t.Fatalf("unexpected historical snapshot: %#v", items)
	}
	if items[0].NetScore != nil {
		t.Fatalf("legacy performance was treated as network score: %#v", items[0].NetScore)
	}
}

func TestProbeParserIgnoresOtherNonceAndRejectsMalformedOutput(t *testing.T) {
	items, complete, err := parseProbeOutput("[DST-ADMIN-PLAYERS other DONE]\n", "wanted")
	if err != nil || complete || len(items) != 0 {
		t.Fatalf("foreign probe leaked into result: complete=%v items=%#v err=%v", complete, items, err)
	}
	if _, _, err := parseProbeOutput("[DST-ADMIN-PLAYERS wanted ITEM] too-few-fields\n", "wanted"); err == nil {
		t.Fatal("malformed probe output was accepted")
	}
}

func TestParseNativePlayerLifecycle(t *testing.T) {
	authenticated, ok := parseAuthenticatedClient("[00:25:18]: Client authenticated: (KU_ONE) Willow The Brave")
	if !ok || authenticated.ID != "KU_ONE" || authenticated.Name != "Willow The Brave" {
		t.Fatalf("authentication was not parsed: %#v ok=%v", authenticated, ok)
	}
	initialized, ok := parseInitializedClient("[00:25:22]: [ClientObject] Initialized (authenticated) on server: guid=123 userid=KU_ONE netid=7656119 admin=1")
	if !ok || initialized.ID != "KU_ONE" || initialized.NetID != "7656119" || !initialized.Admin {
		t.Fatalf("client details were not parsed: %#v ok=%v", initialized, ok)
	}
	if _, ok := parseInitializedClient("[00:00:12]: [ClientObject] Initialized (self/server object, locally trusted) on server: userid=KU_ONE netid= admin=1"); ok {
		t.Fatal("dedicated host identity was treated as a connected player")
	}
	id, prefab, ok := parsePlayerOwnership("[00:26:38]: User ID\tKU_ONE\tassigned ownership to entity\t119979 - wendy\t")
	if !ok || id != "KU_ONE" || prefab != "wendy" {
		t.Fatalf("player ownership was not parsed: id=%q prefab=%q ok=%v", id, prefab, ok)
	}
	id, ok = parseDisconnectedClient("[01:04:08]: [Shard] (KU_ONE) disconnected from Master(1)")
	if !ok || id != "KU_ONE" {
		t.Fatalf("disconnect was not parsed: id=%q ok=%v", id, ok)
	}
}

type nativeProbeCatalog struct {
	room  rooms.Room
	world rooms.World
}

func (c nativeProbeCatalog) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}

func (c nativeProbeCatalog) World(roomID, worldID string) (rooms.World, error) {
	if roomID != c.room.ID || worldID != c.world.ID {
		return rooms.World{}, rooms.ErrWorldNotFound
	}
	return c.world, nil
}

func TestLogProbeReadsNativeEventsIncrementallyWithoutSendingCommands(t *testing.T) {
	root := t.TempDir()
	catalog := nativeProbeCatalog{
		room:  rooms.Room{ID: "room", DirectoryName: "Cluster_1"},
		world: rooms.World{ID: "master", RoomID: "room", DirectoryName: "Master"},
	}
	logDirectory := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(logDirectory, 0750); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDirectory, "server_log.txt")
	joined := strings.Join([]string{
		"[00:25:18]: Client authenticated: (KU_ONE) lcy",
		"[00:25:22]: [ClientObject] Initialized (authenticated) on server: guid=123 userid=KU_ONE netid=7656119 admin=1",
		"[00:26:38]: User ID\tKU_ONE\tassigned ownership to entity\t119979 - wendy\t",
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(joined), 0640); err != nil {
		t.Fatal(err)
	}
	probe, err := NewLogProbe(root, catalog)
	if err != nil {
		t.Fatal(err)
	}
	items, err := probe.Snapshot(context.Background(), "room", "master")
	if err != nil || len(items) != 1 || items[0].ID != "KU_ONE" || items[0].Name != "lcy" || items[0].Prefab != "wendy" || items[0].NetID != "7656119" || !items[0].Admin {
		t.Fatalf("native join snapshot failed: items=%#v err=%v", items, err)
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("[01:04:08]: [Shard] (KU_ONE) disconnected from Master(1)\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	items, err = probe.Snapshot(context.Background(), "room", "master")
	if err != nil || len(items) != 0 {
		t.Fatalf("native disconnect snapshot failed: items=%#v err=%v", items, err)
	}
	history, err := probe.HistorySnapshot(context.Background(), "room", "master")
	if err != nil || len(history) != 1 || history[0].Name != "lcy" || history[0].Prefab != "wendy" {
		t.Fatalf("native history was not retained: items=%#v err=%v", history, err)
	}
}
