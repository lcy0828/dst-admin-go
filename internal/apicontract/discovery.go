package apicontract

import (
	"sort"
	"strings"
)

// DiscoveryFields makes data documented in a native schema discoverable even
// without a separately curated list of feature aliases. It is built once per
// immutable operation, bounded and never includes live user data.
func (op *Operation) DiscoveryFields() string {
	op.discoveryOnce.Do(func() {
		parts := []string{}
		budget := 8192
		add := func(s string) {
			if len(s) > budget || budget <= 0 {
				return
			}
			parts = append(parts, s)
			budget -= len(s) + 1
		}
		seen := map[string]bool{}
		var visit func(map[string]any, int)
		visit = func(schema map[string]any, depth int) {
			if depth > 6 || budget <= 0 {
				return
			}
			if ref, _ := schema["$ref"].(string); ref != "" {
				if seen[ref] {
					return
				}
				seen[ref] = true
			}
			schema = op.catalog.dereference(schema)
			for _, key := range []string{"title", "description"} {
				if v, ok := schema[key].(string); ok {
					add(v)
				}
			}
			props, _ := schema["properties"].(map[string]any)
			keys := make([]string, 0, len(props))
			for k := range props {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				add(key)
				child, _ := props[key].(map[string]any)
				description, _ := op.catalog.dereference(child)["description"].(string)
				if budget > 0 && len(op.discoveryEntries) < 128 {
					entry := []rune(key + ": " + description)
					op.discoveryEntries = append(op.discoveryEntries, string(entry[:min(len(entry), 160)]))
				}
				visit(child, depth+1)
			}
			if child, ok := schema["items"].(map[string]any); ok {
				visit(child, depth+1)
			}
			for _, key := range []string{"allOf", "anyOf", "oneOf"} {
				if branches, ok := schema[key].([]any); ok {
					for _, branch := range branches {
						child, _ := branch.(map[string]any)
						visit(child, depth+1)
					}
				}
			}
		}
		for _, p := range op.Parameters {
			add(p.Name)
			add(p.Description)
			visit(p.Schema, 0)
		}
		visit(op.RequestBody.Content["application/json"].Schema, 0)
		// Mutation responses often reuse the entire object being changed. Those
		// fields do not make killing a player a way to inspect their network.
		if op.ExecutionPolicy().Access != "read" {
			op.discoveryText = strings.Join(parts, " ")
			return
		}
		keys := make([]string, 0, len(op.Responses))
		for key := range op.Responses {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, status := range keys {
			if strings.HasPrefix(status, "2") {
				if r, e := op.response(status); e == nil {
					schema := op.businessSchema(r)
					if schema == nil {
						schema = r.Content["application/json"].Schema
					}
					visit(schema, 0)
				}
			}
		}
		op.discoveryText = strings.Join(parts, " ")
	})
	return op.discoveryText
}

// MatchingFields explains schema matches without sending the entire response
// schema to the model. Tokens come from the same index used for discovery.
func (op *Operation) MatchingFields(tokens []string) []string {
	op.DiscoveryFields()
	type match struct {
		text  string
		score int
	}
	rows := []match{}
	for _, entry := range op.discoveryEntries {
		score := 0
		seen := map[string]bool{}
		for _, token := range tokens {
			if len([]rune(token)) >= 2 && !seen[token] && strings.Contains(strings.ToLower(entry), strings.ToLower(token)) {
				seen[token] = true
				score += len([]rune(token))
			}
		}
		if score > 0 {
			rows = append(rows, match{entry, score})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	result := []string{}
	for _, row := range rows[:min(4, len(rows))] {
		result = append(result, row.text)
	}
	return result
}
