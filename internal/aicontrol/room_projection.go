package aicontrol

import "encoding/json"

func filterRoomList(p Principal, data json.RawMessage) (json.RawMessage, error) {
	if !p.Restricted() {
		return data, nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	var items []map[string]any
	if err := json.Unmarshal(value["items"], &items); err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, row := range items {
		id, _ := row["id"].(string)
		if p.AllowsRoom(id) {
			rows = append(rows, row)
		}
	}
	value["items"], _ = json.Marshal(rows)
	value["total"], _ = json.Marshal(len(rows))
	return json.Marshal(value)
}
func filterAPIList(p Principal, data json.RawMessage) (json.RawMessage, error) {
	if !p.Restricted() {
		return data, nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	projected, err := filterRoomList(p, value["data"])
	if err != nil {
		return nil, err
	}
	value["data"] = projected
	return json.Marshal(value)
}
