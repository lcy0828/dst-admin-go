package apicontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShippedContractGeneratesEveryOperation(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Operations()) < 290 {
		t.Fatal("management operations disappeared", len(c.Operations()))
	}
	for _, op := range c.Operations() {
		t.Run(op.ID, func(t *testing.T) {
			if _, err := op.ToolSchema(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, id := range []string{"chatWithAI", "login", "setupAdmin", "logout", "getAuthSession"} {
		op, ok := c.Operation(id)
		if !ok || op.Callable() {
			t.Fatalf("transport entry exposed as a management tool: %s", id)
		}
	}
}

func TestSchemaRejectsUnknownFieldsAndEscapingPaths(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	op, ok := c.Operation("updateRoomBackupPolicy")
	if !ok {
		t.Fatal("backup operation missing")
	}
	valid := `{"path":{"roomId":"room-a"},"body":{"enabled":true,"intervalMinutes":60,"maxSnapshots":7},"requestId":"backup-0001"}`
	in, err := op.Validate(json.RawMessage(valid))
	if err != nil {
		t.Fatal(err)
	}
	if uri, err := op.URL(in); err != nil || uri != "/api/v2/rooms/room-a/backup-policy" {
		t.Fatal(uri, err)
	}
	for _, raw := range []string{strings.Replace(valid, `"enabled":true`, `"enabled":"yes"`, 1), strings.Replace(valid, `"maxSnapshots":7`, `"maxSnapshots":0`, 1), strings.Replace(valid, `"body":{`, `"url":"http://outside.invalid","body":{`, 1)} {
		if _, err := op.Validate(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid input accepted: %s", raw)
		}
	}
	in.Path["roomId"] = "../other"
	if _, err := op.URL(in); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestEntityDirectoryToolsExposeWorkingPagination(t *testing.T) {
	catalog, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"getEntityDirectory", "readWorldEntitySnapshot"} {
		op, ok := catalog.Operation(id)
		if !ok {
			t.Fatal("missing directory operation", id)
		}
		schema, err := op.ToolSchema()
		encoded, _ := json.Marshal(schema)
		if err != nil || !strings.Contains(string(encoded), `"limit"`) || !strings.Contains(string(encoded), `"offset"`) || !strings.Contains(string(encoded), `"q"`) {
			t.Fatal("tool still advertises no pagination", id, err)
		}
		input := `{"query":{"q":"gold","offset":60,"limit":60}}`
		if id == "readWorldEntitySnapshot" {
			input = `{"path":{"roomId":"room","worldId":"Master"},"query":{"q":"gold","offset":60,"limit":60}}`
		}
		parsed, err := op.Validate(json.RawMessage(input))
		if err != nil {
			t.Fatal(id, err)
		}
		uri, err := op.URL(parsed)
		if err != nil || !strings.Contains(uri, "limit=60") || !strings.Contains(uri, "offset=60") || !strings.Contains(uri, "q=gold") {
			t.Fatal(id, uri, err)
		}
		if _, err := op.Validate(json.RawMessage(strings.Replace(input, `"limit":60`, `"limit":121`, 1))); err == nil {
			t.Fatal("tool accepted an invalid page size", id)
		}
	}
}
