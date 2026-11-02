package apicontract

import (
	"encoding/json"
	"testing"
)

func TestChatStreamContractAndDiscovery(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	op, ok := c.Operation("streamRoomChatLogs")
	if !ok || op.Transport != "stream" || op.Mutation() || op.ToolMetadata().Access != "read" || op.ToolMetadata().Destructive {
		t.Fatal("wrong stream contract")
	}
	for _, raw := range []string{`{"path":{"roomId":"example"}}`, `{"path":{"roomId":"example"},"query":{"kind":"announcement","cursor":"opaque"}}`, `{"path":{"roomId":"example"},"headers":{"Last-Event-ID":"opaque"}}`} {
		if _, err := op.Validate(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`{"path":{"roomId":"example"},"query":{"kind":"unknown"}}`, `{"path":{"roomId":"example"},"query":{"cursor":""}}`, `{"path":"/server/save"}`, `{"path":{"roomId":"example"},"body":{"command":"print(1)"}}`} {
		if _, err := op.Validate(json.RawMessage(raw)); err == nil {
			t.Fatal("accepted invalid input", raw)
		}
	}
	found := false
	for _, item := range c.Search("模组公告聊天推送", 12) {
		if item.ID == op.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("chat stream undiscoverable")
	}
}
