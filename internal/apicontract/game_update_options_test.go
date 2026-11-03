package apicontract

import (
	"encoding/json"
	"testing"
)

func TestGameUpdateSingleConfirmationAndLegacyMCPContracts(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	op, ok := c.Operation("publishGameRelease")
	if !ok || !op.Mutation() || op.ToolMetadata().Access != "write" {
		t.Fatal("update lost write permissions")
	}
	validate := func(operation *Operation, raw string) error {
		var input map[string]any
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return err
		}
		input["requestId"] = "test-game-update-options"
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		_, err = operation.Validate(encoded)
		return err
	}
	for _, raw := range []string{
		`{"body":{"confirmed":true,"targetIds":["local"],"policy":{"stopDelaySeconds":0,"skipBackup":true}}}`,
		`{"body":{"confirmed":true,"policy":{"stopDelaySeconds":360}}}`,
		`{"body":{"planHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","confirmation":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
	} {
		if err := validate(op, raw); err != nil {
			t.Fatalf("valid client rejected: %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"body":{}}`, `{"body":{"confirmed":false}}`,
		`{"body":{"confirmed":true,"policy":{"stopDelaySeconds":361}}}`,
		`{"body":{"confirmed":true,"policy":{"stopDelaySeconds":-1}}}`,
		`{"body":{"confirmed":true,"policy":{"stopDelaySeconds":1.5}}}`,
		`{"body":{"confirmed":true,"policy":{"skipBackup":"yes"}}}`,
	} {
		if err := validate(op, raw); err == nil {
			t.Fatalf("accepted invalid update options: %s", raw)
		}
	}
	retry, _ := c.Operation("retryGameRelease")
	for _, raw := range []string{
		`{"path":{"releaseId":"example-release"},"body":{"confirmed":true,"skipBackup":true}}`,
		`{"path":{"releaseId":"example-release"},"body":{"confirmed":true,"cancelUpdate":true}}`,
	} {
		if err := validate(retry, raw); err != nil {
			t.Fatalf("backup decision not exposed to MCP: %s: %v", raw, err)
		}
	}
}
