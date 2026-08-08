package tmux

import "testing"

func TestV2SessionNameRoundTripAvoidsUnderscoreCollisions(t *testing.T) {
	first := V2SessionName("room_a", "b")
	second := V2SessionName("room", "a_b")
	if first == second {
		t.Fatalf("v2 session names collided: %s", first)
	}
	parsed := ParseSessionName(first)
	if !parsed.Valid || parsed.ClusterName != "room_a" || parsed.ShardName != "b" {
		t.Fatalf("round trip failed: %#v", parsed)
	}
}
