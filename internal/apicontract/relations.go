package apicontract

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResultRead is generated from server-owned result schemas. Values come from
// the response, but operation IDs and argument placement never come from logs.
type ResultRead struct {
	OperationID string         `json:"operationId"`
	Arguments   map[string]any `json:"arguments"`
	Match       map[string]any `json:"match,omitempty"`
}

type ResultLink struct {
	OperationID string            `yaml:"operationId"`
	Parameters  map[string]string `yaml:"parameters"`
	Match       map[string]string `yaml:"match"`
}

func ResultValue(value any, path string) any {
	if len(path) > 255 {
		return nil
	}
	for _, part := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[part]
	}
	return value
}

func (op *Operation) TaskState(raw json.RawMessage) string {
	value, _ := op.resultHint(raw, "x-dst-task-state")
	return value
}

func (op *Operation) TaskOutcome(raw json.RawMessage) (string, bool) {
	return op.resultHint(raw, "x-dst-task-outcome")
}

func (op *Operation) resultHint(raw json.RawMessage, hint string) (string, bool) {
	var envelope struct {
		HTTPStatus int            `json:"httpStatus"`
		Data       map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return "", false
	}
	response, err := op.response(fmt.Sprint(envelope.HTTPStatus))
	if err != nil {
		return "", false
	}
	schema := op.catalog.dereference(op.businessSchema(response))
	field, declared := schema[hint].(string)
	value, _ := ResultValue(envelope.Data, field).(string)
	return value, declared
}

// ResultReads resolves x-dst-read on a scalar identity in a shared result type.
// GET references only; the ordinary operation and scope guards run again later.
func (op *Operation) ResultReads(raw json.RawMessage, input Input) []ResultRead {
	var envelope struct {
		HTTPStatus int `json:"httpStatus"`
		Data       any `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	response, err := op.response(fmt.Sprint(envelope.HTTPStatus))
	if err != nil {
		return nil
	}
	out := []ResultRead{}
	var source any
	_ = json.Unmarshal(raw, &source)
	for _, link := range op.Followups {
		target, ok := op.catalog.Operation(link.OperationID)
		if !ok || !target.Callable() || target.Method != "GET" || target.ExecutionPolicy().Access != "read" {
			continue
		}
		args := map[string]any{}
		match := map[string]any{}
		valid := true
		for pointer, path := range link.Parameters {
			section, field, ok := strings.Cut(pointer, ".")
			value := ResultValue(source, path)
			if !ok || (section != "path" && section != "query") || value == nil {
				valid = false
				break
			}
			fields, _ := args[section].(map[string]any)
			if fields == nil {
				fields = map[string]any{}
				args[section] = fields
			}
			fields[field] = value
		}
		for pointer, path := range link.Match {
			value := ResultValue(source, path)
			if value == nil {
				valid = false
				break
			}
			match[pointer] = value
		}
		encoded, _ := json.Marshal(args)
		if _, err := target.Validate(encoded); valid && err == nil {
			out = append(out, ResultRead{OperationID: link.OperationID, Arguments: args, Match: match})
		}
	}
	var visit func(map[string]any, any, int)
	visit = func(schema map[string]any, value any, depth int) {
		if depth > 8 || len(out) >= 8 {
			return
		}
		schema = op.catalog.dereference(schema)
		if hint, ok := schema["x-dst-read"].(map[string]any); ok {
			id, _ := hint["operationId"].(string)
			parameter, _ := hint["parameter"].(string)
			target, exists := op.catalog.Operation(id)
			section, field, valid := strings.Cut(parameter, ".")
			if exists && target.Callable() && target.Method == "GET" && target.ExecutionPolicy().Access == "read" && valid && (section == "path" || section == "query") && value != nil {
				args := map[string]any{section: map[string]any{field: value}}
				for _, p := range target.Parameters {
					if p.In == "path" && p.Name != field && input.Path[p.Name] != nil {
						m, _ := args["path"].(map[string]any)
						if m == nil {
							m = map[string]any{}
							args["path"] = m
						}
						m[p.Name] = input.Path[p.Name]
					}
				}
				encoded, _ := json.Marshal(args)
				if _, e := target.Validate(encoded); e == nil {
					out = append(out, ResultRead{OperationID: id, Arguments: args})
				}
			}
		}
		if object, ok := value.(map[string]any); ok {
			props, _ := schema["properties"].(map[string]any)
			for key, child := range props {
				if v, found := object[key]; found {
					definition, _ := child.(map[string]any)
					visit(definition, v, depth+1)
				}
			}
		}
	}
	visit(op.businessSchema(response), envelope.Data, 0)
	return out
}
