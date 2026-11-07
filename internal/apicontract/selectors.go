package apicontract

// Selectors compiles reusable references from native parameter/schema metadata.
// Legacy common ID names remain supported; new names use x-dst-reference once
// in the shared schema. Separate occurrences keep separate parameter roles.
func (op *Operation) Selectors() map[string]string {
	out := map[string]string{}
	known := map[string]string{"roomId": "room", "worldId": "world", "playerId": "player"}
	for _, p := range op.Parameters {
		role, _ := p.Schema["x-dst-reference"].(string)
		if role == "" {
			role = known[p.Name]
		}
		if role != "" {
			out[p.In+"."+p.Name] = role
		}
	}
	var visit func(map[string]any, string, int)
	visit = func(schema map[string]any, path string, depth int) {
		if depth > 8 {
			return
		}
		schema = op.catalog.dereference(schema)
		if role, ok := schema["x-dst-reference"].(string); ok {
			out[path] = role
		}
		props, _ := schema["properties"].(map[string]any)
		for field, child := range props {
			definition, _ := child.(map[string]any)
			visit(definition, path+"."+field, depth+1)
		}
	}
	visit(op.RequestBody.Content["application/json"].Schema, "body", 0)
	return out
}

func (op *Operation) RequiredSelectors() []string {
	out := []string{}
	selectors := op.Selectors()
	for _, p := range op.Parameters {
		pointer := p.In + "." + p.Name
		if p.Required && selectors[pointer] != "" {
			out = append(out, pointer)
		}
	}
	var visit func(map[string]any, string, bool, int)
	visit = func(schema map[string]any, path string, required bool, depth int) {
		if depth > 8 {
			return
		}
		schema = op.catalog.dereference(schema)
		if required && selectors[path] != "" {
			out = append(out, path)
		}
		fields, _ := schema["required"].([]any)
		names := map[string]bool{}
		for _, field := range fields {
			if name, ok := field.(string); ok {
				names[name] = true
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for key, value := range props {
			child, _ := value.(map[string]any)
			visit(child, path+"."+key, required && names[key], depth+1)
		}
	}
	visit(op.RequestBody.Content["application/json"].Schema, "body", op.RequestBody.Required, 0)
	return out
}
