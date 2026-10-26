package playerinventory

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"dont/shared"
)

type itemReader struct {
	result               *shared.PlayerInventory
	seen                 map[*table]string
	nodes, checks, bytes int
}

func field(t *table, key string) any {
	if t == nil {
		return nil
	}
	return (*t)[key]
}
func child(t *table, key string) *table { v, _ := field(t, key).(*table); return v }
func text(v any, limit int) string {
	s, _ := v.(string)
	if len(s) > limit {
		s = s[:limit]
		for !utf8.ValidString(s) && len(s) > 0 {
			s = s[:len(s)-1]
		}
	}
	return strings.ToValidUTF8(s, "")
}
func number(v any) *float64 {
	n, ok := v.(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}

func normalize(root *table, result *shared.PlayerInventory) error {
	result.Character = text(field(root, "prefab"), 128)
	inv := child(child(root, "data"), "inventory")
	if inv == nil {
		return failure("SAVE_FORMAT_UNSUPPORTED")
	}
	r := itemReader{result: result, seen: map[*table]string{}}
	result.Equipment = r.slots(child(inv, "equip"), "equipment", 0)
	result.Inventory = r.slots(child(inv, "items"), "inventory", 0)
	if active := child(inv, "activeitem"); active != nil {
		result.ActiveItem = r.item(active, "active", 0)
	} else if field(inv, "activeitem") != nil {
		r.limit("INVALID_ITEM")
	}
	if child(inv, "equip") == nil || child(inv, "items") == nil {
		r.limit("ROOT_SECTION_UNAVAILABLE")
	}
	result.Empty = result.Status == "complete" && len(result.Equipment) == 0 && len(result.Inventory) == 0 && result.ActiveItem == nil
	return nil
}

func (r *itemReader) limit(reason string) {
	r.result.Status = "partial"
	for _, v := range r.result.LimitsReached {
		if v == reason {
			return
		}
	}
	r.result.LimitsReached = append(r.result.LimitsReached, reason)
}

func (r *itemReader) slots(t *table, path string, depth int) []shared.InventoryItem {
	items := []shared.InventoryItem{}
	if t == nil {
		return items
	}
	if depth > 6 {
		r.limit("DEPTH_LIMIT")
		return items
	}
	type slot struct {
		key     any
		name    string
		index   float64
		numeric bool
	}
	keys := make([]slot, 0, len(*t))
	for k := range *t {
		r.checks++
		if r.checks > 4096 {
			r.limit("WORK_LIMIT")
			break
		}
		s := slot{key: k}
		switch v := k.(type) {
		case string:
			s.name = text(v, 64)
		case float64:
			s.numeric = true
			s.index = v
			s.name = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			r.limit("INVALID_SLOT")
			continue
		}
		keys = append(keys, s)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].numeric && keys[j].numeric {
			return keys[i].index < keys[j].index
		}
		return keys[i].name < keys[j].name
	})
	for _, s := range keys {
		v := (*t)[s.key]
		if v == nil {
			continue
		}
		record, ok := v.(*table)
		if !ok {
			r.limit("INVALID_ITEM")
			continue
		}
		item := r.item(record, path+"/"+s.name, depth)
		if item == nil {
			continue
		}
		item.Slot = s.name
		items = append(items, *item)
	}
	return items
}

func (r *itemReader) item(record *table, path string, depth int) *shared.InventoryItem {
	if depth > 6 {
		r.limit("DEPTH_LIMIT")
		return nil
	}
	if r.nodes >= 512 || r.bytes >= 160<<10 {
		r.limit("ITEM_LIMIT")
		return nil
	}
	if _, ok := r.seen[record]; ok {
		r.limit("REPEATED_REFERENCE")
		return nil
	}
	r.nodes++
	r.seen[record] = path
	prefab := text(field(record, "prefab"), 128)
	if prefab == "" {
		r.limit("INVALID_ITEM")
		return nil
	}
	data := child(record, "data")
	name := text(field(child(data, "named"), "name"), 256)
	nameSource := "saved_name"
	if name == "" {
		name = prefab
		nameSource = "prefab"
	}
	cost := 1024 + 6*(len(path)+len(prefab)+len(name))
	if r.bytes+cost > 160<<10 {
		r.limit("BYTE_LIMIT")
		return nil
	}
	one := float64(1)
	item := &shared.InventoryItem{NodeID: path, Slot: path, Prefab: prefab, Name: name, NameSource: nameSource, Count: &one, RecordSource: "serialized", Contents: []shared.InventoryItem{}, Metrics: map[string]shared.InventoryMetric{}}
	if stack := child(data, "stackable"); stack != nil {
		if v := field(stack, "stack"); v != nil {
			item.Count = number(v)
			if item.Count == nil || *item.Count < 1 {
				item.Count = nil
				item.Warnings = append(item.Warnings, "INVALID_COUNT")
			}
		}
	}
	for _, m := range []struct{ name, component, key, unit string }{{"perishable", "perishable", "time", "seconds"}, {"fuel", "fueled", "fuel", "fuel"}, {"armor", "armor", "condition", "condition"}, {"uses", "finiteuses", "uses", "uses"}} {
		metric := shared.InventoryMetric{Unit: m.unit, Availability: "not_saved", Reason: "MAXIMUM_NOT_SAVED"}
		metric.Value = number(field(child(data, m.component), m.key))
		if metric.Value != nil {
			metric.Availability = "value_only"
		}
		item.Metrics[m.name] = metric
	}
	r.bytes += cost
	if c := child(data, "container"); c != nil {
		item.ContainerKind = "container"
		item.Contents = r.slots(child(c, "items"), path+"/container", depth+1)
	}
	if c := child(data, "unwrappable"); c != nil {
		item.ContainerKind = "bundle"
		item.Contents = append(item.Contents, r.slots(child(c, "items"), path+"/bundle", depth+1)...)
	}
	if c := child(data, "inventoryitemholder"); c != nil {
		item.ContainerKind = "holder"
		if held := child(c, "item"); held != nil {
			if v := r.item(held, path+"/holder", depth+1); v != nil {
				item.Contents = append(item.Contents, *v)
			}
		}
	}
	return item
}
