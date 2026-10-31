package players

import (
	"context"
	"testing"

	"dont/internal/playerinventory"
)

func TestOnlineWorldUsesShardLocalEvidenceWithoutInventoryOrMutation(t *testing.T) {
	s, reader, sender := newInventoryLocalityService(t, 0, "caves")
	world, err := s.OnlineWorld(context.Background(), "room", inventoryTestPlayer)
	if err != nil || world.ID != "caves" || len(sender.calls) != 0 || len(reader.refreshes) != 0 {
		t.Fatal(world, err, sender.calls, reader.refreshes)
	}
	for name, value := range reader.values {
		value.Players[0].GameplayState = GameplayStateMigrating
		reader.values[name] = value
	}
	_, err = s.OnlineWorld(context.Background(), "room", inventoryTestPlayer)
	if playerinventory.Code(err) != "PLAYER_LOCATION_UNCERTAIN" || len(sender.calls) != 0 {
		t.Fatal(err, sender.calls)
	}
}
