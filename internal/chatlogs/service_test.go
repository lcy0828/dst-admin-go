package chatlogs

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/logstream"
	"dont/internal/rooms"
)

type chatCatalog struct{ managed bool }

func (c chatCatalog) Room(string) (rooms.Room, error) {
	return rooms.Room{ID: "room", Name: "Room", Managed: c.managed}, nil
}

type chatSnapshots struct{ value logstream.RoomSnapshot }

func (s chatSnapshots) RoomChatSnapshot(context.Context, string, int, string) (logstream.RoomSnapshot, error) {
	return s.value, nil
}

func TestListParsesFiltersAndDeduplicatesShardChatLogs(t *testing.T) {
	now := time.Now().UTC()
	masterStartedAt := time.Date(2026, time.August, 21, 1, 0, 0, 0, time.UTC)
	cavesStartedAt := masterStartedAt.Add(5 * time.Second)
	snapshots := chatSnapshots{value: logstream.RoomSnapshot{
		RoomID: "room", ReadAt: now, Available: 2, Worlds: []logstream.WorldSnapshot{
			{
				WorldID: "master", WorldName: "地面", WorldRole: rooms.WorldRoleMaster,
				Snapshot: &logstream.Snapshot{FileName: "server_chat_log.txt", StartedAt: masterStartedAt, UpdatedAt: now, Lines: []logstream.Line{
					{Cursor: 10, Text: "[00:00:06]: [Say] (KU_ONE) Willow: hello"},
					{Cursor: 20, Text: "[00:00:09]: [Say] (KU_ONE) Willow: hello"},
					{Cursor: 30, Text: "[00:00:20]: [Whisper] (KU_TWO) Wendy: secret"},
					{Cursor: 40, Text: "[00:00:30]: [Join Announcement] Willow"},
				}},
			},
			{
				WorldID: "caves", WorldName: "洞穴", WorldRole: rooms.WorldRoleCaves,
				Snapshot: &logstream.Snapshot{FileName: "server_chat_log.txt", StartedAt: cavesStartedAt, UpdatedAt: now.Add(-time.Second), Lines: []logstream.Line{
					{Cursor: 10, Text: "[00:00:01]: [Say] (KU_ONE) Willow: hello"},
					{Cursor: 20, Text: "[00:00:04]: [Say] (KU_ONE) Willow: hello"},
					{Cursor: 30, Text: "[00:00:15]: [Whisper] (KU_TWO) Wendy: secret"},
					{Cursor: 40, Text: "[00:00:25]: [Join Announcement] Willow"},
				}},
			},
		},
	}}
	service, err := NewService(chatCatalog{managed: true}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room", Filter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 4 || len(list.Items) != 4 || list.Counts[KindSay] != 2 || list.Counts[KindWhisper] != 1 || list.Counts[KindAnnouncement] != 1 {
		t.Fatalf("chat list=%#v", list)
	}
	for _, entry := range list.Items {
		if len(entry.Sources) != 2 || entry.Sources[0].WorldRole != rooms.WorldRoleMaster {
			t.Fatalf("deduplicated sources=%#v", entry.Sources)
		}
	}
	if list.Items[0].Kind != KindAnnouncement || list.Items[1].Kind != KindWhisper {
		t.Fatalf("newest-first items=%#v", list.Items)
	}
	if list.StartedAt == nil || !list.StartedAt.Equal(masterStartedAt) {
		t.Fatalf("room startedAt=%v want=%s", list.StartedAt, masterStartedAt)
	}
	if list.Items[0].OccurredAt == nil || !list.Items[0].OccurredAt.Equal(masterStartedAt.Add(30*time.Second)) {
		t.Fatalf("announcement occurredAt=%v", list.Items[0].OccurredAt)
	}

	filtered, err := service.List(context.Background(), "room", Filter{Query: "secret", Kind: KindWhisper, WorldID: "caves", Limit: 10})
	if err != nil || filtered.Total != 1 || filtered.Items[0].PlayerName != "Wendy" {
		t.Fatalf("filtered=%#v err=%v", filtered, err)
	}
}

func TestListReportsPartialSnapshotsAndRejectsInvalidFilters(t *testing.T) {
	now := time.Now().UTC()
	service, err := NewService(chatCatalog{managed: true}, chatSnapshots{value: logstream.RoomSnapshot{
		RoomID: "room", ReadAt: now, Partial: true, Available: 1, Unavailable: 1,
		Worlds: []logstream.WorldSnapshot{
			{WorldID: "master", WorldName: "地面", WorldRole: rooms.WorldRoleMaster, Snapshot: &logstream.Snapshot{UpdatedAt: now, Truncated: true}},
			{WorldID: "caves", WorldName: "洞穴", WorldRole: rooms.WorldRoleCaves, Problem: &logstream.Problem{Code: "LOG_NOT_FOUND", Message: "missing"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room", Filter{})
	if err != nil || !list.Partial || !list.Truncated || len(list.Problems) != 1 || list.Limit != defaultLimit {
		t.Fatalf("partial list=%#v err=%v", list, err)
	}
	if _, err := service.List(context.Background(), "room", Filter{Kind: Kind("invalid")}); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("invalid filter error=%v", err)
	}
}

func TestListKeepsShardSpecificWhispersWhileMergingSharedMessages(t *testing.T) {
	now := time.Now().UTC()
	service, err := NewService(chatCatalog{managed: true}, chatSnapshots{value: logstream.RoomSnapshot{
		RoomID: "room", ReadAt: now, Available: 2,
		Worlds: []logstream.WorldSnapshot{
			{
				WorldID: "master", WorldName: "地面", WorldRole: rooms.WorldRoleMaster,
				Snapshot: &logstream.Snapshot{UpdatedAt: now, Lines: []logstream.Line{
					{Text: "[00:00:10]: [Say] (KU_ONE) Willow: shared"},
					{Text: "[00:00:20]: [Whisper] (KU_ONE) Willow: master only"},
				}},
			},
			{
				WorldID: "caves", WorldName: "洞穴", WorldRole: rooms.WorldRoleCaves,
				Snapshot: &logstream.Snapshot{UpdatedAt: now.Add(-time.Second), Lines: []logstream.Line{
					{Text: "[00:00:05]: [Say] (KU_ONE) Willow: shared"},
					{Text: "[00:00:15]: [Whisper] (KU_TWO) Wendy: caves only"},
				}},
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room", Filter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 3 || list.Counts[KindSay] != 1 || list.Counts[KindWhisper] != 2 {
		t.Fatalf("chat list=%#v", list)
	}
	if list.Items[0].Content != "master only" || list.Items[1].Content != "caves only" || list.Items[2].Content != "shared" {
		t.Fatalf("chat items=%#v", list.Items)
	}
	if len(list.Items[2].Sources) != 2 || list.Items[2].Sources[0].WorldRole != rooms.WorldRoleMaster {
		t.Fatalf("shared sources=%#v", list.Items[2].Sources)
	}
}

func TestListRequiresManagedRoom(t *testing.T) {
	service, err := NewService(chatCatalog{managed: false}, chatSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), "room", Filter{}); !errors.Is(err, ErrRoomNotManaged) {
		t.Fatalf("managed room error=%v", err)
	}
}
