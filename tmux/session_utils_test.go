package tmux

import (
	"reflect"
	"testing"
)

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

func TestConsoleSendArgumentsKeepLuaAsOneLiteralArgument(t *testing.T) {
	command := `dst_admin_custom("; ' 中文 Enter")`
	want := []string{
		"send-keys", "-t", "=managed:0.0", "C-q", "C-u", ";",
		"send-keys", "-t", "=managed:0.0", "-l", "--", command, ";",
		"send-keys", "-t", "=managed:0.0", "Enter",
	}
	if got := consoleSendArguments("=managed:0.0", command); !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments=%#v", got)
	}
}
