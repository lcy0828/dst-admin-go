package entitycatalog

import "strings"

type PageOptions struct {
	Query    string
	Offset   int
	Limit    int
	Complete bool
}

// PageEntities filters cached metadata without new game commands or an extra
// full-size copy. The workbench explicitly requests Complete for local ranking;
// API/MCP callers get a bounded page by default.
func PageEntities(items []Entity, options PageOptions) ([]Entity, int, error) {
	query := strings.ToLower(strings.TrimSpace(options.Query))
	if len([]rune(query)) > 100 || options.Offset < 0 || options.Offset > 50000 || options.Limit < 1 || options.Limit > 120 || options.Complete && options.Offset != 0 {
		return nil, 0, ErrInvalidSearch
	}
	limit := options.Limit
	if options.Complete {
		limit = len(items)
	}
	if query == "" {
		if len(items) == 0 {
			return []Entity{}, 0, nil
		}
		start := min(options.Offset, len(items))
		return items[start:min(start+limit, len(items))], len(items), nil
	}
	page := make([]Entity, 0, min(limit, len(items)))
	total := 0
	for _, item := range items {
		if query != "" && !strings.Contains(strings.ToLower(item.ID), query) &&
			!strings.Contains(strings.ToLower(item.NameZhCN), query) &&
			!strings.Contains(strings.ToLower(item.NameEn), query) &&
			!strings.Contains(strings.ToLower(item.ModName), query) {
			continue
		}
		if total >= options.Offset && len(page) < limit {
			page = append(page, item)
		}
		total++
	}
	return page, total, nil
}
