package aicontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"dont/internal/apicontract"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpMetadataKey = "io.github.lcy0828.dst-admin/tool"
const dispatchAPI = "call_api"
const mcpPageSize = 64

type mcpViewKey struct{}
type mcpView struct {
	View   string
	Filter apicontract.MetadataFilter
}

func parseMCPView(query url.Values) (mcpView, error) {
	view := mcpView{View: query.Get("view"), Filter: apicontract.MetadataFilter{Access: query.Get("access"), Risk: query.Get("risk"), Runtime: query.Get("runtime")}}
	if view.View == "" {
		view.View = "compact"
	}
	if !slices.Contains([]string{"compact", "all", "legacy"}, view.View) {
		return view, ErrInvalid
	}
	for _, tag := range query["tags"] {
		for _, v := range strings.Split(tag, ",") {
			view.Filter.Tags = append(view.Filter.Tags, strings.ToLower(strings.TrimSpace(v)))
		}
	}
	slices.Sort(view.Filter.Tags)
	view.Filter.Tags = slices.Compact(view.Filter.Tags)
	return view, view.Filter.Validate()
}
func apiDispatcherTool() Tool {
	return Tool{Name: dispatchAPI, Mutation: true, Description: "Execute a management operation discovered by find_api. First call find_api with includeSchema=true and inspect inputSchema. Pass its operationId and exact arguments (path/query/body/requestId as declared). All input validation, credential scope, idempotency and Runtime guards remain active. Check submitted jobs; never replay uncertain mutations.", InputSchema: schema([]string{"operationId", "arguments"}, map[string]any{"operationId": map[string]any{"type": "string", "minLength": 1, "maxLength": 64}, "arguments": map[string]any{"type": "object", "additionalProperties": true}})}
}
func (s *Service) dispatchMCP(ctx context.Context, p Principal, raw json.RawMessage) (Invocation, error) {
	if len(raw) > 64<<10 || s.catalog == nil {
		return Invocation{}, ErrInvalid
	}
	var in struct {
		OperationID string          `json:"operationId"`
		Arguments   json.RawMessage `json:"arguments"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || len(in.Arguments) == 0 {
		return Invocation{}, ErrInvalid
	}
	op, ok := s.catalog.Operation(in.OperationID)
	if !ok || !op.Callable() {
		return Invocation{}, ErrDenied
	}
	return s.Invoke(ctx, p, op.ToolName(), in.Arguments)
}

func (s *Service) mcpDefinition(tool Tool) *mcp.Tool {
	metadata := apicontract.ToolMetadata{Title: tool.Name, Tags: []string{"system"}, Access: "read", Risk: "low", Runtime: "any", Idempotent: !tool.Mutation, Destructive: tool.Mutation}
	if tool.Mutation {
		metadata.Access = "write"
		metadata.Risk = "high"
	}
	if tool.Metadata != nil {
		metadata = *tool.Metadata
	} else {
		// Compatibility helpers keep their actual aggregate behavior. Tags are
		// explicit rather than inferred from public names in each client.
		domains := map[string][]string{
			"list_rooms":        {"room"},
			"room_status":       {"room", "runtime"},
			"runtime_modes":     {"room", "runtime"},
			"world_logs":        {"room", "world", "log"},
			"room_events":       {"room", "runtime", "log"},
			"latest_diagnostic": {"room", "world", "runtime"},
			"world_state":       {"room", "world"},
			"room_players":      {"room", "player"},
			"room_mods":         {"room", "mod"},
			"room_backups":      {"room", "backup", "schedule"},
			"room_automation":   {"room", "schedule"},
			"list_jobs":         {"job"}, "get_job": {"job"},
			"call_result":      {"ai"},
			"command_receipt":  {"room", "console"},
			"room_performance": {"room", "world", "runtime"},
			"room_control":     {"room", "runtime"},
			"world_console":    {"room", "world", "console"},
			discoverAPI:        {"discovery"}, dispatchAPI: {"discovery"},
		}
		if tags, ok := domains[tool.Name]; ok {
			metadata.Tags = tags
		}
		if tool.Name == "world_console" {
			metadata.Runtime = "shard-ready"
		}
	}
	extension := map[string]any{"tags": metadata.Tags, "access": metadata.Access, "risk": metadata.Risk, "runtime": metadata.Runtime}
	if s.catalog != nil {
		if op, ok := s.catalog.Operation(tool.Name); ok {
			extension["deprecated"] = op.Deprecated
			extension["replacedBy"] = op.ReplacedBy
		}
	}
	return &mcp.Tool{Name: tool.Name, Title: metadata.Title, Description: tool.Description, InputSchema: tool.InputSchema, Meta: mcp.Meta{mcpMetadataKey: extension}, Annotations: &mcp.ToolAnnotations{Title: metadata.Title, ReadOnlyHint: metadata.Access == "read", IdempotentHint: metadata.Idempotent, DestructiveHint: &metadata.Destructive, OpenWorldHint: &metadata.OpenWorld}}
}

var compactMCPNames = map[string]bool{discoverAPI: true, dispatchAPI: true, "api_listRooms": true, "api_getRoomRuntimeOverview": true, "api_listRoomPlayers": true, "api_listRoomMods": true, "api_listRoomBackupSchedules": true, "api_listJobs": true, "api_getJob": true}

// Listing views are immutable projections of one shared server. No per-token
// server cache or tool registration is needed, and hidden tools remain callable.
func (s *Service) catalogMiddleware(definitions []*mcp.Tool) mcp.Middleware {
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	encoded, _ := json.Marshal(definitions)
	catalogHash := sha256.Sum256(encoded)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}
			view, ok := ctx.Value(mcpViewKey{}).(mcpView)
			if !ok {
				view = mcpView{View: "compact"}
			}
			explicitFilter := len(view.Filter.Tags) > 0 || view.Filter.Access != "" || view.Filter.Risk != "" || view.Filter.Runtime != ""
			selected := make([]*mcp.Tool, 0, len(definitions))
			for _, definition := range definitions {
				if s.catalog != nil {
					op, isAPI := s.catalog.Operation(definition.Name)
					if view.View == "all" || explicitFilter && view.View != "legacy" {
						if !isAPI || op.Deprecated {
							continue
						}
					} else if view.View == "compact" && !compactMCPNames[definition.Name] {
						continue
					}
				}
				info := definition.Meta[mcpMetadataKey].(map[string]any)
				metadata := apicontract.ToolMetadata{Tags: info["tags"].([]string), Access: info["access"].(string), Risk: info["risk"].(string), Runtime: info["runtime"].(string)}
				if view.Filter.Matches(metadata) {
					selected = append(selected, definition)
				}
			}
			scope, _ := json.Marshal(view)
			hash := sha256.Sum256(append(catalogHash[:], scope...))
			fingerprint := hex.EncodeToString(hash[:12])
			offset := 0
			if params, ok := req.GetParams().(*mcp.ListToolsParams); ok && params != nil && params.Cursor != "" {
				raw, err := base64.RawURLEncoding.DecodeString(params.Cursor)
				if err != nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid tool cursor"}
				}
				prefix, value, ok := strings.Cut(string(raw), ":")
				parsed, err := strconv.Atoi(value)
				if !ok || prefix != fingerprint || err != nil || parsed < 1 || parsed >= len(selected) || parsed%mcpPageSize != 0 {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tool cursor does not match this catalog view"}
				}
				offset = parsed
			}
			end := min(offset+mcpPageSize, len(selected))
			result := &mcp.ListToolsResult{Tools: selected[offset:end], Meta: mcp.Meta{"io.github.lcy0828.dst-admin/catalog": map[string]any{"view": view.View, "total": len(selected), "pageSize": mcpPageSize, "catalog": fingerprint}}}
			if end < len(selected) {
				result.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(fingerprint + ":" + strconv.Itoa(end)))
			}
			return result, nil
		}
	}
}
