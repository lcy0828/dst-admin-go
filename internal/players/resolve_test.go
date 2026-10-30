package players

import (
	"dont/internal/rooms"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestResolveIdentityNeverBreaksTiesByOnlineStatus(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Now()
	if err := store.ReplaceWorldSnapshot("room", "master", "Master", []Observation{
		{ID: "KU_one", Name: "小白"}, {ID: "KU_two", Name: "小白"}, {ID: "KU_emoji", Name: "\U000f0001玩家 A 🦌"}, {ID: "KU_like", Name: "100%_玩家"},
	}, now); err != nil {
		t.Fatal(err)
	}
	s := &Service{store: store, rooms: playerTestCatalog{room: rooms.Room{ID: "room", Name: "Room", Managed: true}}}
	tests := []struct {
		query, status, match string
		total                int
	}{
		{"KU_one", "resolved", "exact_id", 1}, {"小白", "ambiguous", "exact_name", 2},
		{"玩家 A 🦌", "resolved", "normalized_name", 1}, {"玩家", "ambiguous", "substring", 2},
		{"100%_玩家", "resolved", "exact_name", 1}, {"missing", "not_found", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			got, err := s.Resolve("room", tc.query, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status || got.Match != tc.match || got.Total != tc.total {
				t.Fatalf("%+v", got)
			}
		})
	}
	for _, q := range []string{"", strings.Repeat("a", 129), "bad\nname", "\x00"} {
		if _, err := s.Resolve("room", q, 0); err == nil {
			t.Fatalf("accepted invalid %q", q)
		}
	}
	if _, err := s.Resolve("another-room", "小白", 0); err == nil {
		t.Fatal("cross-room lookup accepted")
	}
}

func TestResolvePaginationNormalizationCollisionAndBoundedDirectory(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Now()
	observations := []Observation{
		{ID: "KU_ascii", Name: "A\u200b 玩家"}, {ID: "KU_full", Name: "Ａ 玩家"},
	}
	for i := range 7 {
		observations = append(observations, Observation{ID: fmt.Sprintf("KU_group_%d", i), Name: "同名玩家"})
	}
	if err := store.ReplaceWorldSnapshot("room", "master", "Master", observations, now); err != nil {
		t.Fatal(err)
	}
	s := &Service{store: store, rooms: playerTestCatalog{room: rooms.Room{ID: "room", Managed: true}}}
	first, err := s.Resolve("room", "同名玩家", 0)
	if err != nil || first.Status != "ambiguous" || first.Total != 7 || len(first.Items) != 5 || !first.HasMore {
		t.Fatalf("first page=%+v error=%v", first, err)
	}
	second, err := s.Resolve("room", "同名玩家", 5)
	if err != nil || len(second.Items) != 2 || second.HasMore || first.Items[4].ID == second.Items[0].ID {
		t.Fatalf("second page=%+v error=%v", second, err)
	}
	for _, query := range []string{"a 玩家", "' OR 1=1 --", "%_"} {
		got, err := s.Resolve("room", query, 0)
		if err != nil {
			t.Fatal(err)
		}
		if query == "a 玩家" {
			if got.Status != "ambiguous" || got.Match != "normalized_name" || got.Total != 2 {
				t.Fatalf("normalization collision resolved arbitrarily: %+v", got)
			}
		} else if got.Status != "not_found" {
			t.Fatalf("nonliteral query: %q %+v", query, got)
		}
	}
	// Seed the cardinality boundary in one SQLite statement; thousands of ORM
	// inserts would make this small bounded-read regression unnecessarily slow.
	if err := store.db.Exec(`WITH RECURSIVE seq(n) AS (
		SELECT 0 UNION ALL SELECT n+1 FROM seq WHERE n<4096
	) INSERT INTO ` + store.table + `
	(room_id, user_id, world_id, world_name, name, online, admin,
	first_seen_at, last_seen_at, status_changed_at, last_refreshed_at)
	SELECT 'room', printf('KU_large_%04d', n), 'master', 'Master', '大目录玩家', 0, 0,
	CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM seq`).Error; err != nil {
		t.Fatal(err)
	}
	limited, err := s.Resolve("room", "同名玩家", 0)
	if err != nil || limited.Status != "unavailable" || limited.Coverage != "directory_limit" || len(limited.Items) != 0 {
		t.Fatalf("unbounded directory=%+v error=%v", limited, err)
	}
	exact, err := s.Resolve("room", "KU_large_4096", 0)
	if err != nil || exact.Status != "resolved" || exact.Match != "exact_id" || len(exact.Items) != 1 {
		t.Fatalf("large-directory stable ID lookup=%+v error=%v", exact, err)
	}
	for _, offset := range []int{-1, 4097} {
		if _, err := s.Resolve("room", "KU_ascii", offset); err == nil {
			t.Fatalf("invalid offset %d accepted", offset)
		}
	}
}
