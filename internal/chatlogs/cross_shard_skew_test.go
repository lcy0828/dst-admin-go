package chatlogs

import (
	"dont/internal/rooms"
	"dont/shared"
	"fmt"
	"testing"
	"time"
)

// Replay both collection orders with drift larger than the previous window.
// Identical consecutive messages are separate; each gains exactly two sources.
func TestCrossShardClockSkewPreservesRepeatedPublicChat(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch=%v/reverse=%v", batch, reverse), func(t *testing.T) {
				store := newChatStore(t)
				start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				worlds := []rooms.World{{ID: "master", Role: rooms.WorldRoleMaster}, {ID: "caves", Role: rooms.WorldRoleCaves}}
				if reverse {
					worlds[0], worlds[1] = worlds[1], worlds[0]
				}
				add := func(world rooms.World, indices []int) {
					entries := []parsedEntry{}
					for _, i := range indices {
						skew := 0
						if world.ID == "caves" {
							skew = 12
						}
						entries = append(entries, parsedChatEntry(t, "room", world, start, int64(100+i*50), fmt.Sprintf("[00:01:%02d]: [Say] (KU_EXAMPLE) Example: repeated", i*9+skew)))
					}
					generation := shared.RuntimeChatLogGeneration{ID: world.ID, Size: 1000, StartedAt: start}
					if _, err := store.ImportGeneration("room", world, generation, entries, 1000, start.Add(2*time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
				if batch {
					for _, w := range worlds {
						add(w, []int{0, 1, 2})
					}
				} else {
					for i := 0; i < 3; i++ {
						for _, w := range worlds {
							add(w, []int{i})
						}
					}
				}
				list, err := store.List("room", Filter{Limit: 20})
				if err != nil || list.Total != 3 {
					t.Fatalf("%+v %v", list, err)
				}
				for _, item := range list.Items {
					if len(item.Sources) != 2 {
						t.Fatalf("lost mirrored source: %+v", item)
					}
				}
				// Reimporting all records must emit no new entries or changes.
				for _, w := range worlds {
					add(w, []int{0, 1, 2})
				}
				replay, err := store.List("room", Filter{Limit: 20})
				if err != nil || replay.Total != 3 {
					t.Fatalf("replay: %+v %v", replay, err)
				}
			})
		}
	}
}
