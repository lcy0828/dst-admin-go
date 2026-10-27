package apicontract

import (
	"fmt"
	"strings"
)

// standaloneSchema copies only reachable definitions and rewrites component
// references. Both input and result documentation use the same resolver.
func (c *Catalog) standaloneSchema(root map[string]any) (map[string]any, error) {
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
						definition, exists := c.schemas[name]
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
		return nil, err
	}
	out := resolved.(map[string]any)
	if len(defs) > 0 {
		out["$defs"] = defs
	}
	return out, nil
}
