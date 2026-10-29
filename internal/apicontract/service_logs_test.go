package apicontract

import (
	"dont/shared"
	"encoding/json"
	"testing"
)

func TestServiceLogToolsAreDiscoverableAndValidateSourceFilters(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"queryServiceLogs", "listServiceLogSources", "getServiceLogSource", "getServiceLogContext", "getServiceLogDiagnostics", "getServiceLogSettings", "updateServiceLogSettings", "exportServiceLogs"} {
		op, ok := c.Operation(id)
		if !ok || !op.Callable() || op.Semantics() == nil {
			t.Fatal("tool missing", id)
		}
	}
	op, _ := c.Operation("queryServiceLogs")
	input, err := op.Validate(json.RawMessage(`{"query":{"sourceId":"agent:node","jobId":"failed-job","limit":100}}`))
	if err != nil {
		t.Fatal(err)
	}
	url, err := op.URL(input)
	if err != nil || url != "/api/v2/system/service-logs?jobId=failed-job&limit=100&sourceId=agent%3Anode" {
		t.Fatal(url, err)
	}
	if _, err := op.Validate(json.RawMessage(`{"query":{"path":"/etc/passwd"}}`)); err == nil {
		t.Fatal("arbitrary path accepted")
	}
	found := false
	for _, v := range c.SearchFiltered("面板后台报错服务日志", 12, MetadataFilter{Tags: []string{"log"}}) {
		if v.ID == "queryServiceLogs" {
			found = true
		}
	}
	if !found {
		t.Fatal("Chinese discovery did not find panel logs")
	}
}

func TestCachedServiceLogSourcesMatchTheirResultSchema(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	op, _ := c.Operation("listServiceLogSources")
	response := op.Responses["200"]
	fake := &Operation{ID: "response", Method: "GET", catalog: c, RequestBody: Body{Required: true, Content: map[string]Media{"application/json": {Schema: response.Content["application/json"].Schema}}}}
	sample := shared.ServiceLogSource{ID: "agent:node", Name: "Node", NodeID: "agent:node", Online: true, Supported: true}
	raw, _ := json.Marshal(map[string]any{"body": map[string]any{"data": map[string]any{"items": []shared.ServiceLogSource{sample}}, "error": nil, "meta": map[string]any{"requestId": "sample"}}})
	if _, err := fake.Validate(raw); err != nil {
		t.Fatal(err, string(raw))
	}
}
