package runtimeguard

import (
	"errors"
	"testing"

	"dont/internal/rooms"
)

type guardWorlds struct {
	items []rooms.World
}

func (c guardWorlds) Worlds(string) ([]rooms.World, error) {
	return append([]rooms.World(nil), c.items...), nil
}

type guardPlacements struct {
	local map[string]bool
}

func (p guardPlacements) IsLocalPlacement(_, worldID string) (bool, error) {
	return p.local[worldID], nil
}

func TestRequireRoomRejectsAnyRemoteAppliedPlacement(t *testing.T) {
	guard, err := New(
		guardWorlds{items: []rooms.World{{ID: "master"}, {ID: "caves"}}},
		guardPlacements{local: map[string]bool{"master": true, "caves": false}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.RequireRoom("room"); !errors.Is(err, ErrRemoteMutationUnavailable) {
		t.Fatalf("RequireRoom error = %v", err)
	}
}

func TestRequireRoomAllowsAllLocalAppliedPlacements(t *testing.T) {
	guard, err := New(
		guardWorlds{items: []rooms.World{{ID: "master"}, {ID: "caves"}}},
		guardPlacements{local: map[string]bool{"master": true, "caves": true}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.RequireRoom("room"); err != nil {
		t.Fatalf("RequireRoom error = %v", err)
	}
}
