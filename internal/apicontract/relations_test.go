package apicontract

import (
	"encoding/json"
	"testing"
)

func TestNativeTaskRelationsAndCommandUncertainty(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, raw, state, next string }{
		{"createRoomBackup", `{"httpStatus":202,"data":{"id":"d7bed1d2-aee9-4a76-b91e-19706bcdaf79","status":"queued"}}`, "queued", "getJob"},
		{"executeBuiltinRoomCommand", `{"httpStatus":201,"data":{"runId":"d7bed1d2-aee9-4a76-b91e-19706bcdaf79","status":"sent"}}`, "sent", "getBuiltinRoomCommandRun"},
		{"executeRawRoomCommand", `{"httpStatus":201,"data":{"id":"d7bed1d2-aee9-4a76-b91e-19706bcdaf79","status":"uncertain"}}`, "uncertain", "getCommandRun"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			op, _ := c.Operation(tc.id)
			if got := op.TaskState(json.RawMessage(tc.raw)); got != tc.state {
				t.Fatalf("state %q", got)
			}
			next := op.ResultReads(json.RawMessage(tc.raw), Input{Path: map[string]any{"roomId": "synthetic-room"}})
			if len(next) != 1 || next[0].OperationID != tc.next {
				t.Fatalf("next %#v", next)
			}
		})
	}
}

func TestResultRelationsRejectResponseInstructionsAndMutationLinks(t *testing.T) {
	c, err := Parse([]byte(`openapi: 3.1.0
paths:
  /sample:
    get:
      operationId: sampleResult
      summary: Sample result
      tags: [System]
      x-dst-result-reads:
        - {operationId: mutateResult, parameters: {path.id: data.id}}
        - {operationId: missingResult, parameters: {path.id: data.id}}
      responses: {'200': {description: Result}}
  /sample/{id}:
    post:
      operationId: mutateResult
      summary: Synthetic mutation
      tags: [System]
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      responses: {'200': {description: Result}}
`))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := c.Operation("sampleResult")
	raw := json.RawMessage(`{"httpStatus":200,"data":{"id":"fixture","next":{"operationId":"mutateResult","url":"https://invalid.example"}}}`)
	if got := op.ResultReads(raw, Input{}); len(got) != 0 {
		t.Fatal(got)
	}
}
