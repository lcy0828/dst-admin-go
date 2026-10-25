// Package apicontract derives executable, typed operations from the shipped
// OpenAPI contract. It performs no network or background work.
package apicontract

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	apidocs "dont/docs"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

const ToolPrefix = "api_"

type Parameter struct {
	Name        string         `yaml:"name"`
	In          string         `yaml:"in"`
	Required    bool           `yaml:"required"`
	Description string         `yaml:"description"`
	Schema      map[string]any `yaml:"schema"`
	Ref         string         `yaml:"$ref"`
	Explode     *bool          `yaml:"explode"`
}

type Media struct {
	Schema map[string]any `yaml:"schema"`
}
type Body struct {
	Required bool             `yaml:"required"`
	Content  map[string]Media `yaml:"content"`
}
type Response struct {
	Description string           `yaml:"description"`
	Content     map[string]Media `yaml:"content"`
	Ref         string           `yaml:"$ref"`
}
type Operation struct {
	ID          string              `yaml:"operationId" json:"operationId"`
	Summary     string              `yaml:"summary" json:"summary"`
	Description string              `yaml:"description" json:"description,omitempty"`
	Deprecated  bool                `yaml:"deprecated" json:"deprecated,omitempty"`
	ReplacedBy  string              `yaml:"x-dst-replaced-by" json:"replacedBy,omitempty"`
	Tags        []string            `yaml:"tags" json:"tags"`
	Parameters  []Parameter         `yaml:"parameters" json:"-"`
	RequestBody Body                `yaml:"requestBody" json:"-"`
	Responses   map[string]Response `yaml:"responses" json:"-"`
	Transport   string              `yaml:"x-dst-transport" json:"transport"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	catalog     *Catalog
	once        sync.Once
	input       map[string]any
	validator   *jsonschema.Resolved
	err         error
	searchText  string
	searchID    string
	searchTags  []string
	aiOnce      sync.Once
	aiSchema    map[string]any
	aiErr       error
}
type Catalog struct {
	operations map[string]*Operation
	ordered    []*Operation
	schemas    map[string]any
	parameters map[string]Parameter
	responses  map[string]Response
	paths      map[string]map[string]any
}

var Default = sync.OnceValues(func() (*Catalog, error) { return Parse(apidocs.OpenAPI) })

func Parse(data []byte) (*Catalog, error) {
	var doc struct {
		OpenAPI    string                    `yaml:"openapi"`
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas    map[string]any       `yaml:"schemas"`
			Parameters map[string]Parameter `yaml:"parameters"`
			Responses  map[string]Response  `yaml:"responses"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.OpenAPI != "3.1.0" || len(doc.Paths) == 0 {
		return nil, fmt.Errorf("OpenAPI 3.1 contract is required")
	}
	c := &Catalog{operations: map[string]*Operation{}, schemas: doc.Components.Schemas, parameters: doc.Components.Parameters, responses: doc.Components.Responses, paths: doc.Paths}
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
			return nil, fmt.Errorf("invalid API path %q", path)
		}
		var common []Parameter
		if v, ok := item["parameters"]; ok {
			if err := decode(v, &common); err != nil {
				return nil, err
			}
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete", "head", "options"} {
			raw, ok := item[method]
			if !ok {
				continue
			}
			op := &Operation{Method: strings.ToUpper(method), Path: path, catalog: c}
			if err := decode(raw, op); err != nil {
				return nil, err
			}
			if op.ID == "" || len(ToolPrefix+op.ID) > 64 || c.operations[op.ID] != nil {
				return nil, fmt.Errorf("missing, duplicate or oversized operationId: %s %s %q", method, path, op.ID)
			}
			for _, ch := range op.ID {
				if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
					return nil, fmt.Errorf("invalid operationId %q", op.ID)
				}
			}
			op.Parameters = append(common, op.Parameters...)
			for i, p := range op.Parameters {
				if p.Ref != "" {
					name := strings.TrimPrefix(p.Ref, "#/components/parameters/")
					resolved, ok := c.parameters[name]
					if !ok {
						return nil, fmt.Errorf("unresolved parameter %q", p.Ref)
					}
					op.Parameters[i] = resolved
				}
			}
			if op.Transport == "" {
				op.Transport = op.inferTransport()
			}
			switch op.Transport {
			case "json", "multipart", "download", "stream", "identity", "assistant":
			default:
				return nil, fmt.Errorf("invalid transport for %s: %s", op.ID, op.Transport)
			}
			c.operations[op.ID] = op
			op.searchID = strings.ToLower(op.ID)
			op.searchText = strings.ToLower(strings.Join(op.Tags, " ") + " " + op.ID + " " + op.Path + " " + op.Summary + " " + op.Description)
			for _, tag := range op.Tags {
				op.searchTags = append(op.searchTags, strings.ToLower(tag))
			}
			c.ordered = append(c.ordered, op)
		}
	}
	sort.Slice(c.ordered, func(i, j int) bool { return c.ordered[i].ID < c.ordered[j].ID })
	for _, op := range c.ordered {
		if op.ReplacedBy != "" {
			replacement, ok := c.operations[op.ReplacedBy]
			if !ok || replacement == op || !replacement.Callable() || replacement.Deprecated {
				return nil, fmt.Errorf("invalid replacement for %s: %s", op.ID, op.ReplacedBy)
			}
		}
	}
	return c, nil
}

func decode(value any, out any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, out)
}
func (c *Catalog) Operations() []*Operation { return append([]*Operation(nil), c.ordered...) }
func (c *Catalog) Operation(id string) (*Operation, bool) {
	op, ok := c.operations[strings.TrimPrefix(id, ToolPrefix)]
	return op, ok
}
func (op *Operation) ToolName() string { return ToolPrefix + op.ID }
func (op *Operation) Mutation() bool {
	return op.Method != "GET" && op.Method != "HEAD" && op.Method != "OPTIONS"
}
func (op *Operation) Callable() bool {
	return op.Transport != "identity" && op.Transport != "assistant"
}
func (op *Operation) ContentType() string {
	for _, kind := range []string{"application/json", "multipart/form-data", "application/octet-stream"} {
		if _, ok := op.RequestBody.Content[kind]; ok {
			return kind
		}
	}
	return ""
}
func (op *Operation) inferTransport() string {
	if op.ContentType() == "multipart/form-data" {
		return "multipart"
	}
	for status, r := range op.Responses {
		if !strings.HasPrefix(status, "2") {
			continue
		}
		if r.Ref != "" {
			r = op.catalog.responses[strings.TrimPrefix(r.Ref, "#/components/responses/")]
		}
		if _, ok := r.Content["text/event-stream"]; ok {
			return "stream"
		}
		for kind := range r.Content {
			if kind != "application/json" {
				return "download"
			}
		}
	}
	return "json"
}

// ToolSchema includes only reachable component schemas. The contract remains
// authoritative; this adapter groups HTTP locations for model/MCP arguments.
func (op *Operation) ToolSchema() (map[string]any, error) {
	op.once.Do(op.compile)
	return op.input, op.err
}
func (op *Operation) compile() {
	root := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	props := root["properties"].(map[string]any)
	required := []string{}
	for _, location := range []string{"path", "query", "header"} {
		p := map[string]any{}
		req := []string{}
		for _, param := range op.Parameters {
			if param.In != location || location == "header" && injectedHeader(param.Name) {
				continue
			}
			definition := param.Schema
			if definition == nil {
				definition = map[string]any{"type": "string"}
			}
			if param.Description != "" {
				copy := make(map[string]any, len(definition)+1)
				for key, value := range definition {
					copy[key] = value
				}
				copy["description"] = param.Description
				definition = copy
			}
			p[param.Name] = definition
			if param.Required {
				req = append(req, param.Name)
			}
		}
		if len(p) > 0 {
			key := location
			if key == "header" {
				key = "headers"
			}
			props[key] = map[string]any{"type": "object", "properties": p, "required": req, "additionalProperties": false}
			if len(req) > 0 {
				required = append(required, key)
			}
		}
	}
	if kind := op.ContentType(); kind != "" {
		definition := op.RequestBody.Content[kind].Schema
		if kind == "multipart/form-data" {
			definition = op.catalog.multipartSchema(definition)
		}
		props["body"] = definition
		if op.RequestBody.Required {
			required = append(required, "body")
		}
	}
	if op.Mutation() {
		props["requestId"] = map[string]any{"type": "string", "minLength": 8, "maxLength": 128, "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$", "description": "Stable idempotency key; reuse only for retries of this same operation."}
		required = append(required, "requestId")
	}
	root["required"] = required
	defs := map[string]any{}
	var rewrite func(any) (any, error)
	rewrite = func(value any) (any, error) {
		switch v := value.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, item := range v {
				if k == "$ref" {
					ref, ok := item.(string)
					if !ok || !strings.HasPrefix(ref, "#/components/schemas/") {
						return nil, fmt.Errorf("unsupported schema reference %v", item)
					}
					name := strings.TrimPrefix(ref, "#/components/schemas/")
					if _, seen := defs[name]; !seen {
						definition, exists := op.catalog.schemas[name]
						if !exists {
							return nil, fmt.Errorf("unresolved schema %q", name)
						}
						defs[name] = map[string]any{}
						resolved, err := rewrite(definition)
						if err != nil {
							return nil, err
						}
						defs[name] = resolved
					}
					out[k] = "#/$defs/" + name
				} else {
					resolved, err := rewrite(item)
					if err != nil {
						return nil, err
					}
					out[k] = resolved
				}
			}
			return out, nil
		case []any:
			out := make([]any, len(v))
			for i, item := range v {
				r, err := rewrite(item)
				if err != nil {
					return nil, err
				}
				out[i] = r
			}
			return out, nil
		default:
			return value, nil
		}
	}
	resolved, err := rewrite(root)
	if err != nil {
		op.err = err
		return
	}
	op.input = resolved.(map[string]any)
	if len(defs) > 0 {
		op.input["$defs"] = defs
	}
	encoded, err := json.Marshal(op.input)
	if err != nil {
		op.err = err
		return
	}
	var schema jsonschema.Schema
	if err = json.Unmarshal(encoded, &schema); err != nil {
		op.err = err
		return
	}
	op.validator, op.err = schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
}

func injectedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "cookie", "x-csrf-token", "idempotency-key", "x-dst-runtime-target":
		return true
	}
	return false
}

func (c *Catalog) multipartSchema(value map[string]any) map[string]any {
	if ref, ok := value["$ref"].(string); ok {
		if v, ok := c.schemas[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any); ok {
			return c.multipartSchema(v)
		}
	}
	out := map[string]any{}
	for k, v := range value {
		out[k] = v
	}
	if properties, ok := value["properties"].(map[string]any); ok {
		p := map[string]any{}
		for name, v := range properties {
			if field, ok := v.(map[string]any); ok && field["format"] == "binary" {
				p[name] = map[string]any{"type": "object", "required": []string{"attachmentId"}, "additionalProperties": false, "properties": map[string]any{"attachmentId": map[string]any{"type": "string", "format": "uuid", "description": "ID of a file already uploaded to this administrator's AI attachments."}}}
			} else {
				p[name] = v
			}
		}
		out["properties"] = p
	}
	return out
}

type Input struct {
	Path      map[string]any `json:"path,omitempty"`
	Query     map[string]any `json:"query,omitempty"`
	Headers   map[string]any `json:"headers,omitempty"`
	Body      any            `json:"body,omitempty"`
	RequestID string         `json:"requestId,omitempty"`
}

func (op *Operation) Validate(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) > 256<<10 {
		return in, fmt.Errorf("operation arguments exceed 256 KiB")
	}
	if _, err := op.ToolSchema(); err != nil {
		return in, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return in, err
	}
	if err := op.validator.Validate(value); err != nil {
		return in, fmt.Errorf("%s arguments: %w", op.ID, err)
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, err
	}
	return in, nil
}

func (op *Operation) URL(in Input) (string, error) {
	path := op.Path
	for _, p := range op.Parameters {
		if p.In != "path" {
			continue
		}
		value, ok := in.Path[p.Name]
		if !ok || value == nil {
			return "", fmt.Errorf("missing path parameter %s", p.Name)
		}
		v := fmt.Sprint(value)
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/\\\x00\r\n") {
			return "", fmt.Errorf("invalid path parameter %s", p.Name)
		}
		path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(v))
	}
	if strings.ContainsAny(path, "{}") {
		return "", fmt.Errorf("unresolved API path")
	}
	query := url.Values{}
	for _, p := range op.Parameters {
		if p.In != "query" {
			continue
		}
		v, ok := in.Query[p.Name]
		if !ok {
			continue
		}
		if list, ok := v.([]any); ok {
			values := make([]string, len(list))
			for i, item := range list {
				values[i] = fmt.Sprint(item)
			}
			if p.Explode != nil && !*p.Explode {
				query.Set(p.Name, strings.Join(values, ","))
			} else {
				for _, item := range values {
					query.Add(p.Name, item)
				}
			}
		} else {
			query.Set(p.Name, fmt.Sprint(v))
		}
	}
	result := "/api/v2" + path
	if len(query) > 0 {
		result += "?" + query.Encode()
	}
	return result, nil
}

// Search uses the complete catalog; matching is deterministic and has no model
// request, vector index or idle worker. Empty query lists all domains.
func (c *Catalog) Search(query string, limit int) []*Operation {
	query = strings.ToLower(strings.TrimSpace(query))
	words := strings.Fields(query)
	type match struct {
		op    *Operation
		score int
	}
	matches := make([]match, 0, len(c.ordered))
	for _, op := range c.ordered {
		if !op.Callable() {
			continue
		}
		score := 0
		identity := op.searchID
		if strings.TrimPrefix(query, ToolPrefix) == identity {
			score = 1000
		}
		hay := op.searchText
		if query == "" {
			score = 1
		} else {
			for _, word := range words {
				if strings.Contains(hay, word) {
					score++
				}
				if strings.Contains(strings.ToLower(op.Summary), word) {
					score += 4
				}
				if strings.Contains(identity, word) {
					score += 3
				}
				for _, tag := range op.searchTags {
					if tag == word {
						score += 2
					}
				}
			}
		}
		if score > 0 {
			if op.Deprecated && score < 1000 {
				score = max(1, score-3)
			}
			matches = append(matches, match{op, score})
		}
	}
	// Equivalent compatibility aliases may mention more search terms in their
	// migration notes. Their recommended operation must still precede them.
	byID := make(map[string]int, len(matches))
	for i, item := range matches {
		byID[item.op.ID] = i
	}
	for _, item := range matches {
		if !item.op.Deprecated || item.score >= 1000 || item.op.ReplacedBy == "" {
			continue
		}
		if index, ok := byID[item.op.ReplacedBy]; ok && matches[index].score < 1000 && matches[index].score <= item.score {
			matches[index].score = item.score + 1
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		// Exact IDs remain discoverable, including compatibility operations.
		if a.score >= 1000 || b.score >= 1000 {
			return a.score > b.score
		}
		if a.score == b.score && a.op.Deprecated != b.op.Deprecated {
			return !a.op.Deprecated
		}
		return a.score > b.score
	})
	if limit < 1 || limit > 30 {
		limit = 8
	}
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]*Operation, len(matches))
	for i, m := range matches {
		out[i] = m.op
	}
	return out
}
