package aicontrol

import (
	"fmt"
	"maps"

	"dont/internal/apicontract"
)

const AccessListsPermissionMessage = "当前密钥可以查询玩家，但不能读取管理名单；查询玩家时请省略 includeAccessLists，或设为 false。"
const playerListPolicyRevision = "player-access-lists-v1"

// PlayerListAccess preserves omission until the authenticated caller is known.
// It never grants room or operation access; those checks must run first.
type PlayerListAccess struct {
	Include      bool
	omittedState string
}

func (a PlayerListAccess) State(available bool) string {
	if !a.Include {
		return a.omittedState
	}
	if available {
		return "available"
	}
	return "unavailable"
}

func (p Principal) ResolvePlayerListAccess(requested *bool) (PlayerListAccess, error) {
	admin := p.Profile == "" || p.Profile == ProfilePanelManage
	if !admin && p.Profile != ProfileGameRead && p.Profile != ProfileGameManage {
		return PlayerListAccess{}, ErrDenied
	}
	if requested == nil {
		return PlayerListAccess{Include: admin, omittedState: "not_permitted"}, nil
	}
	if !*requested {
		return PlayerListAccess{omittedState: "not_requested"}, nil
	}
	if !admin {
		return PlayerListAccess{}, fmt.Errorf("%w: ACCESS_LISTS_PERMISSION_REQUIRED: %s", ErrDenied, AccessListsPermissionMessage)
	}
	return PlayerListAccess{Include: true}, nil
}

func (p Principal) playerListAccess(in apicontract.Input) (PlayerListAccess, error) {
	var requested *bool
	if value, exists := in.Query["includeAccessLists"]; exists {
		boolean, ok := value.(bool)
		if !ok {
			return PlayerListAccess{}, fmt.Errorf("%w: includeAccessLists must be a boolean", ErrInvalid)
		}
		requested = &boolean
	}
	return p.ResolvePlayerListAccess(requested)
}

// Only clone the changed branches: AIInputSchema and registered MCP definitions
// are shared by all callers. Never write a caller-specific default into them.
func playerListInputSchema(p Principal, input map[string]any) map[string]any {
	result := maps.Clone(input)
	properties := maps.Clone(input["properties"].(map[string]any))
	query := maps.Clone(properties["query"].(map[string]any))
	parameters := maps.Clone(query["properties"].(map[string]any))
	access := maps.Clone(parameters["includeAccessLists"].(map[string]any))
	policy, _ := p.ResolvePlayerListAccess(nil)
	access["default"] = policy.Include
	if policy.Include {
		access["description"] = "是否附带管理名单信息；当前身份省略时默认读取。只查询玩家时可设为 false。"
	} else {
		access["enum"] = []any{false}
		access["description"] = "普通玩家查询可省略，默认 false。当前身份不允许读取管理名单，不能设为 true。"
	}
	parameters["includeAccessLists"] = access
	query["properties"] = parameters
	properties["query"] = query
	result["properties"] = properties
	return result
}

func apiInputSchemaFor(p Principal, op *apicontract.Operation) (map[string]any, error) {
	input, err := op.AIInputSchema()
	if err != nil || op.ID != "listRoomPlayers" {
		return input, err
	}
	return playerListInputSchema(p, input), nil
}

func playerListScopeHint(p Principal) string {
	policy, _ := p.ResolvePlayerListAccess(nil)
	if policy.Include {
		return "普通玩家查询可设置 query.includeAccessLists=false；当前身份省略时包含管理名单信息。"
	}
	return "普通玩家查询可省略 query.includeAccessLists，默认 false；当前身份不能请求管理名单。"
}
