package apicontract

import (
	"fmt"
	"sort"
	"strings"
)

// ResultContract documents the API's business data before AI redaction and
// truncation. It must not be used to validate a possibly shortened MCP receipt.
type ResultContract struct {
	DataPath  string                    `json:"dataPath"`
	Notes     string                    `json:"notes"`
	Responses map[string]ResultResponse `json:"responses"`
}
type ResultResponse struct {
	Description string         `json:"description"`
	DataSchema  map[string]any `json:"dataSchema,omitempty"`
}

func (op *Operation) response(status string) (Response, error) {
	r := op.Responses[status]
	if r.Ref != "" {
		var ok bool
		r, ok = op.catalog.responses[strings.TrimPrefix(r.Ref, "#/components/responses/")]
		if !ok {
			return r, fmt.Errorf("unresolved response %s %s", op.ID, status)
		}
	}
	return r, nil
}

func (c *Catalog) dereference(schema map[string]any) map[string]any {
	seen := map[string]bool{}
	for schema != nil {
		ref, ok := schema["$ref"].(string)
		if !ok || seen[ref] {
			return schema
		}
		seen[ref] = true
		schema, _ = c.schemas[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any)
	}
	return nil
}

func (op *Operation) businessSchema(response Response) map[string]any {
	envelope := op.catalog.dereference(response.Content["application/json"].Schema)
	properties, _ := envelope["properties"].(map[string]any)
	data, _ := properties["data"].(map[string]any)
	return data
}

// ResultSummary stays small enough for each discovered tool. Detailed schemas
// are returned only when explicitly requested for one operation.
func (op *Operation) ResultSummary() string {
	if op.Transport == "download" || op.Transport == "stream" {
		return "返回 data.resource 认证链接（url、kind、expiresAt、fetched）；fetched=false 表示尚未读取内容，生成链接不代表下载或订阅完成。"
	}
	fields := map[string]bool{}
	accepted := false
	for status := range op.Responses {
		if !strings.HasPrefix(status, "2") {
			continue
		}
		accepted = accepted || status == "202"
		r, err := op.response(status)
		if err != nil {
			continue
		}
		data := op.catalog.dereference(op.businessSchema(r))
		if data["type"] == "array" {
			items, _ := data["items"].(map[string]any)
			data = op.catalog.dereference(items)
		}
		properties, _ := data["properties"].(map[string]any)
		for key := range properties {
			fields[key] = true
		}
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 10 {
		names = append(names[:10], "…")
	}
	summary := "调用回执含 callId、status；业务结果位于 data.data。"
	if len(names) > 0 {
		summary += "结果或列表项字段包括 " + strings.Join(names, "、") + "。"
	}
	if accepted {
		summary += "HTTP 202 仅表示已提交，需查询任务结果。"
	}
	if s := op.Semantics(); s != nil && s.ResultNotes != "" {
		summary += s.ResultNotes
	}
	return summary
}

func (op *Operation) ResultSchema() (*ResultContract, error) {
	op.resultOnce.Do(func() {
		result := &ResultContract{DataPath: "data.data", Notes: "Schemas describe complete business data before redaction/truncation. Receipt status is tool execution, not asynchronous job completion. Missing or truncated fields are unknown. HTTP errors appear in receipt data.error; transport errors may be MCP isError text.", Responses: map[string]ResultResponse{}}
		if op.Transport == "download" || op.Transport == "stream" {
			result.DataPath = "data.resource"
			result.Notes = op.ResultSummary()
		} else {
			for status := range op.Responses {
				if !strings.HasPrefix(status, "2") {
					continue
				}
				r, err := op.response(status)
				if err != nil {
					op.resultErr = err
					return
				}
				detail := ResultResponse{Description: r.Description}
				if data := op.businessSchema(r); data != nil {
					detail.DataSchema, err = op.catalog.standaloneSchema(data)
					if err != nil {
						op.resultErr = err
						return
					}
				}
				result.Responses[status] = detail
			}
		}
		op.result = result
	})
	return op.result, op.resultErr
}
