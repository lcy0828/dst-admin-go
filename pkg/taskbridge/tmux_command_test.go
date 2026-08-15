package taskbridge

import (
	"context"
	"encoding/base32"
	"errors"
	"testing"
)

type fakeRuntimeConsole struct {
	room    string
	world   string
	command string
}

func (console *fakeRuntimeConsole) Send(_ context.Context, room, world, command string) error {
	console.room, console.world, console.command = room, world, command
	return nil
}

func TestLegacyTaskBridgeRoutesV2SessionThroughRuntimeConsole(t *testing.T) {
	encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	session := "dstserver_v2_" + encoding.EncodeToString([]byte("room_with_underlines")) + "_" + encoding.EncodeToString([]byte("Master"))
	console := &fakeRuntimeConsole{}
	executor := NewTmuxCommandExecutor(nil, "", "", "", "", console)
	if _, err := executor.ExecuteRawCommand(session, "c_save()"); err != nil {
		t.Fatal(err)
	}
	if console.room != "room_with_underlines" || console.world != "Master" || console.command != "c_save()" {
		t.Fatalf("runtime call=%#v", console)
	}
}

func TestLegacyTaskBridgeDoesNotBypassMissingRuntime(t *testing.T) {
	executor := NewTmuxCommandExecutor(nil, "", "", "", "")
	if _, err := executor.ExecuteRawCommand("dstserver_room_Master", "c_save()"); !errors.Is(err, ErrRuntimeConsoleUnavailable) {
		t.Fatalf("missing runtime error=%v", err)
	}
	if _, err := executor.ExecuteRawCommand("dstserver_room_name_Master", "c_save()"); !errors.Is(err, ErrRuntimeConsoleUnavailable) {
		t.Fatalf("ambiguous legacy name error=%v", err)
	}
}
