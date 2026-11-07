package aicontrol

// The companion gateway is a negotiated, trusted transport. Its actor envelope
// is not a model tool argument. Native API execution and receipts remain shared.
import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"dont/internal/apicontract"
	consoleapi "dont/internal/console"
	"dont/internal/intent"
	"github.com/google/uuid"
)

type CompanionActor struct {
	Identity   []string `json:"identity"` // platform, bot, conversation, user
	Profile    string   `json:"profile"`
	Rooms      []string `json:"rooms"`
	Categories []string `json:"categories"`
	Writes     []string `json:"writes"`
	Deny       []string `json:"deny"`
	Workbench  string   `json:"workbench"`
	PanelWide  bool     `json:"panelWide"`
}
type GenericInput struct {
	Actor     CompanionActor `json:"actor"`
	Mode      string         `json:"mode"`
	Query     string         `json:"query,omitempty"`
	Operation string         `json:"operationId,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	RequestID string         `json:"requestId,omitempty"`
	TaskRef   string         `json:"taskRef,omitempty"`
}
type genericRecord struct {
	ID          string `gorm:"primary_key;type:varchar(64)"`
	Source      string `gorm:"type:varchar(64);index"`
	RequestKey  string `gorm:"unique_index;type:varchar(64)"`
	Fingerprint string `gorm:"type:varchar(64)"`
	Operation   string
	Revision    string
	Arguments   string `gorm:"type:text"`
	State       string
	CallID      string
	CreatedAt   time.Time `gorm:"index"`
}

func (s *Store) genericTable() string { return s.taskTable + "_generic_v1" }

var genericAliases = sync.OnceValue(func() map[string][]string {
	out := map[string][]string{}
	for _, c := range BotCapabilities() {
		out[c.Operation] = append(out[c.Operation], c.ID)
	}
	return out
})

func StrictGenericJSON(raw []byte) error { _, err := strictBotJSONLimit(raw, 64<<10, 16); return err }

func (s *Service) CompanionResource(ceiling Principal, a CompanionActor, id string) (Principal, error) {
	if !s.Enabled() {
		return Principal{}, ErrUnavailable
	}
	p, err := s.companionPrincipal(ceiling, a)
	if err != nil {
		return p, err
	}
	resource, err := s.Store.Resource(p, id)
	if err != nil {
		return p, err
	}
	op, ok := s.catalog.Operation(resource.OperationID)
	if !ok || !s.genericAllowed(p, a, op) || op.Method != "GET" {
		return p, ErrDenied
	}
	if room, _ := resource.Input.Path["roomId"].(string); room != "" && !slices.Contains(a.Rooms, room) {
		return p, ErrDenied
	}
	return p, p.CheckAPI(op, resource.Input)
}

func (s *Service) companionPrincipal(ceiling Principal, a CompanionActor) (Principal, error) {
	// Only a full private service connection can delegate. Restricted/direct MCP
	// credentials cannot turn model-supplied roles into additional privileges.
	if ceiling.ID == "" || ceiling.Restricted() || len(a.Identity) != 4 || len(a.Rooms) > 64 || len(a.Categories) > 64 || len(a.Writes) > 256 || len(a.Deny) > 256 || !slices.Contains([]string{ProfileGameRead, ProfileGameManage, ProfilePanelManage}, a.Profile) {
		return Principal{}, ErrDenied
	}
	for _, parts := range [][]string{a.Identity, a.Rooms, a.Categories, a.Writes, a.Deny} {
		for _, part := range parts {
			if part == "" || len(part) > 255 || strings.ContainsAny(part, "\x00\r\n") {
				return Principal{}, ErrInvalid
			}
		}
	}
	if a.Workbench != "" && !slices.Contains([]string{"off", "standard", "all"}, a.Workbench) || a.PanelWide && a.Profile != ProfilePanelManage {
		return Principal{}, ErrDenied
	}
	identity, _ := json.Marshal(a.Identity)
	p := Principal{ID: digest(ceiling.ID + "\x00companion-v1\x00" + string(identity)), AdminID: ceiling.AdminID, Profile: a.Profile, RoomIDs: a.Rooms, catalog: s.catalog}
	// Explicit panel authority permits global resources. Preserve room filters
	// for lists, job ownership and native routes, rather than clearing RoomIDs.
	p.companionPanel = a.PanelWide
	p.companionScope = true
	return p, nil
}
func (s *Service) genericAllowed(p Principal, a CompanionActor, op *apicontract.Operation) bool {
	if !op.Callable() || strings.Contains(op.Path, "/bot/") {
		return false
	}
	policy := op.ExecutionPolicy()
	if policy.Executor != "" && a.Workbench != "all" {
		// A limited workbench role uses the fixed sub-action adapter. Granting
		// one safe action must not expose the aggregate/script executor.
		return false
	}
	ids := []string{op.ID, op.ToolName(), "api:" + op.ID}
	aliases := genericAliases()[op.ID]
	// A grant for one sub-action must never authorize its whole executor.
	if len(aliases) == 1 {
		ids = append(ids, aliases...)
	}
	for _, id := range ids {
		if slices.Contains(a.Deny, id) {
			return false
		}
	}
	for _, id := range aliases {
		if slices.Contains(a.Deny, id) {
			return false
		}
	}
	if !p.AllowsTool(op.ToolName()) {
		return false
	}
	if policy.Access == "read" && !slices.Contains(a.Categories, "*") && !slices.Contains(a.Categories, policy.Category) {
		return false
	}
	if policy.Access == "write" {
		if p.Profile == ProfileGameRead {
			return false
		}
		if !slices.Contains(a.Writes, "*") {
			allowed := false
			for _, id := range ids {
				allowed = allowed || slices.Contains(a.Writes, id)
			}
			if !allowed {
				return false
			}
		}
	}
	return true
}

func (s *Service) Generic(ctx context.Context, ceiling Principal, in GenericInput) (any, error) {
	ctx = context.WithValue(ctx, genericResultKey{}, true)
	if !s.Enabled() || s.catalog == nil {
		return nil, ErrUnavailable
	}
	select {
	case s.taskSlots <- struct{}{}:
		defer func() { <-s.taskSlots }()
	default:
		return nil, ErrBusy
	}
	p, err := s.companionPrincipal(ceiling, in.Actor)
	if err != nil {
		return nil, err
	}
	if in.Mode == "find" {
		if len(in.Query) > 768 {
			return nil, ErrInvalid
		}
		items := []any{}
		documents := []intent.CapabilityDoc{}
		for _, op := range s.catalog.Operations() {
			if !s.genericAllowed(p, in.Actor, op) {
				continue
			}
			documents = append(documents, nativeDiscoveryDocument(op))
		}
		legacy := map[string]BotCapability{}
		for _, c := range BotCapabilities() {
			workbenchGrant := in.Actor.Workbench == "all" || in.Actor.Workbench == "standard" && c.Risk != "critical"
			if c.Operation != "executeBuiltinRoomCommand" || slices.Contains(in.Actor.Deny, c.ID) || p.Profile == ProfileGameRead || !workbenchGrant && !slices.Contains(in.Actor.Writes, c.ID) {
				continue
			}
			if !p.AllowsTool("api_" + c.Operation) {
				continue
			}
			legacy[c.ID] = c
			terms := append([]string{}, c.SearchTerms...)
			for _, action := range c.Actions {
				terms = append(terms, action.ID, action.Title)
			}
			documents = append(documents, intent.CapabilityDoc{ID: c.ID, Title: c.Title, Access: c.Access, SearchTerms: terms, Aliases: c.Aliases, Examples: c.Examples, Negatives: c.Negatives, Prefer: c.Prefer})
		}
		ranked := s.rankDiscovery(ctx, in.Query, documents)
		for _, candidate := range ranked.Candidates {
			if c, ok := legacy[candidate.Capability.ID]; ok {
				items = append(items, map[string]any{"id": c.ID, "title": c.Title, "parameters": modelParameters(c), "parameterHints": c.ParameterHints, "tool": "dst_run", "input": map[string]any{"capability_id": c.ID}, "policy": map[string]any{"access": c.Access, "confirmation": c.Confirm}, "recipe": c})
				if len(items) == 3 {
					break
				}
				continue
			}
			op, ok := s.catalog.Operation(strings.TrimPrefix(candidate.Capability.ID, "api:"))
			if !ok {
				continue
			}
			if !s.genericAllowed(p, in.Actor, op) {
				continue
			}
			schema, e := apiInputSchemaFor(p, op)
			if e != nil {
				return nil, e
			}
			// Request IDs are supplied by trusted transport, never by the model.
			schema = cloneGenericSchema(schema)
			if props, ok := schema["properties"].(map[string]any); ok {
				delete(props, "requestId")
			}
			if required, ok := schema["required"].([]any); ok {
				out := []any{}
				for _, key := range required {
					if key != "requestId" {
						out = append(out, key)
					}
				}
				schema["required"] = out
			}
			selectors := op.Selectors()
			items = append(items, map[string]any{"id": "api:" + op.ID, "operationId": op.ID, "title": op.Semantics().Purpose, "scope": op.Scope(), "policy": op.ExecutionPolicy(), "parameters": schema, "selectors": selectors, "requiredSelectors": op.RequiredSelectors(), "resultSummary": op.ResultSummary(), "matchingFields": op.MatchingFields(intent.Tokenize(in.Query)), "revision": op.ExecutionRevision(), "transport": op.Transport})
			if len(items) == 3 {
				break
			}
		}
		return map[string]any{"items": items, "catalogRevision": s.catalog.Operations()[0].CatalogRevision(), "protocolVersion": 1, "instruction": "使用 parameters 的 path/query/body 原始结构；未提供的配置字段保持省略。无匹配不表示面板没有此功能，可缩小查询范围。"}, nil
	}
	var record genericRecord
	if in.Mode == "recover" {
		if len(in.RequestID) < 8 || len(in.RequestID) > 128 {
			return nil, ErrInvalid
		}
		// A lost HTTP response can be recovered by the transport's original
		// request identity. This path never dispatches a write, even if no record
		// is visible yet (the original request may still be arriving).
		key := digest(p.ID + "\x00" + in.RequestID)
		if e := s.Store.db.Table(s.Store.genericTable()).Where("request_key = ? AND source = ?", key, p.ID).First(&record).Error; e != nil {
			return map[string]any{"state": "uncertain", "summary": "尚未找到原请求的执行回执，请稍后查询；未重新提交。"}, nil
		}
		in.Mode, in.TaskRef = "result", record.ID
	}
	if in.Mode == "execute" || in.Mode == "result" {
		if _, e := uuid.Parse(in.TaskRef); e != nil {
			return nil, ErrInvalid
		}
		if e := s.Store.db.Table(s.Store.genericTable()).Where("id = ? AND source = ?", in.TaskRef, p.ID).First(&record).Error; e != nil {
			if in.Mode == "result" {
				call, err := s.Store.call(in.TaskRef)
				op, ok := s.catalog.Operation(call.Tool)
				if err == nil && call.Source == p.ID && ok && s.genericAllowed(p, in.Actor, op) && (call.RoomID == "" || slices.Contains(in.Actor.Rooms, call.RoomID)) {
					return s.genericReceipt(ctx, p, in.Actor, call.ID, call, true)
				}
			}
			return nil, ErrDenied
		}
		in.Operation = record.Operation
		if json.Unmarshal([]byte(record.Arguments), &in.Arguments) != nil {
			return nil, ErrInvalid
		}
	}
	op, ok := s.catalog.Operation(in.Operation)
	if !ok || !s.genericAllowed(p, in.Actor, op) {
		return nil, ErrDenied
	}
	policy := op.ExecutionPolicy()
	if in.Mode != "read" && in.Mode != "run" && in.Mode != "execute" && in.Mode != "result" {
		return nil, ErrInvalid
	}
	if in.Mode == "read" && policy.Access != "read" || in.Mode == "run" && policy.Access != "write" {
		return nil, ErrDenied
	}
	args := cloneGenericSchema(in.Arguments)
	if args == nil {
		args = map[string]any{}
	}
	if in.Mode == "read" || in.Mode == "run" {
		if _, exists := args["requestId"]; exists {
			return nil, ErrInvalid
		}
		if len(in.RequestID) < 8 || len(in.RequestID) > 128 {
			return nil, ErrInvalid
		}
		if op.Mutation() {
			args["requestId"] = "generic-" + digest(p.ID+"\x00"+in.RequestID+"\x00"+op.ID)
		}
	}
	raw, _ := json.Marshal(args)
	var input apicontract.Input
	var e error
	if in.Mode == "result" || in.Mode == "execute" && record.State != "prepared" {
		// These stored arguments were validated before dispatch. A later schema
		// change or expired private input must not destroy original receipts.
		e = json.Unmarshal(raw, &input)
	} else {
		resolved, privateValues, resolveErr := s.Store.resolvePrivateInputs(p, raw)
		if resolveErr != nil {
			return nil, resolveErr
		}
		input, e = op.Validate(resolved)
		if e != nil && len(privateValues) > 0 {
			return nil, fmt.Errorf("%w: private input does not match the requested API field", ErrInvalid)
		}
	}
	if e != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, e)
	}
	if e = p.CheckAPI(op, input); e != nil {
		return nil, e
	}
	revision := op.ExecutionRevision()
	if policy.Executor == "workbench" {
		revision = digest(revision + "\x00" + consoleapi.WorkbenchBehaviorRevision)
		if in.Mode == "run" || in.Mode == "execute" && record.State == "prepared" {
			body, _ := input.Body.(map[string]any)
			id, _ := body["commandId"].(string)
			arguments, _ := body["arguments"].(map[string]any)
			definition, validation := consoleapi.ValidateBuiltinWorkbench(id, arguments, true)
			if validation != nil {
				return nil, ErrInvalid
			}
			// Only the immutable recipe's actual low-risk policy can skip the
			// aggregate endpoint's confirmation, never caller-supplied flags.
			if definition.Risk == consoleapi.RiskLow && !consoleapi.RequiresHumanConfirmation(id, arguments) {
				policy.Confirmation = "none"
			}
		}
	}
	if room, _ := input.Path["roomId"].(string); room != "" && !slices.Contains(in.Actor.Rooms, room) {
		return nil, ErrDenied
	}
	if in.Mode == "read" {
		result, e := s.Invoke(WithModelChannel(ctx), p, op.ToolName(), raw)
		if e != nil {
			return genericResult("", result), e
		}
		call, e := s.Store.call(result.CallID)
		if e != nil {
			return nil, e
		}
		return s.genericReceipt(ctx, p, in.Actor, call.ID, call, false)
	}
	if in.Mode == "run" {
		// Private values must use existing private-input references. Prepared
		// plans never persist a plaintext credential or unredacted secret.
		for _, value := range op.SensitiveValues(raw) {
			if !strings.HasPrefix(value, privateInputPrefix) {
				return nil, fmt.Errorf("%w: sensitive values require private input references; configure secrets in the authenticated panel", ErrInvalid)
			}
		}
		key := digest(p.ID + "\x00" + in.RequestID)
		fingerprint := digest(op.ID + "\x00" + string(raw))
		s.Store.mu.Lock()
		found := s.Store.db.Table(s.Store.genericTable()).Where("request_key = ?", key).First(&record)
		if found.RecordNotFound() {
			var count int
			e = s.Store.db.Table(s.Store.genericTable()).Where("created_at < ?", time.Now().Add(-30*24*time.Hour)).Delete(&genericRecord{}).Error
			if e == nil {
				e = s.Store.db.Table(s.Store.genericTable()).Count(&count).Error
			}
			if e == nil && count >= 10000 {
				e = ErrBusy
			}
			if e == nil {
				record = genericRecord{ID: uuid.NewString(), Source: p.ID, RequestKey: key, Fingerprint: fingerprint, Operation: op.ID, Revision: revision, Arguments: string(raw), State: "prepared", CreatedAt: time.Now().UTC()}
				e = s.Store.db.Table(s.Store.genericTable()).Create(&record).Error
			}
		} else {
			e = found.Error
			if e == nil && record.Fingerprint != fingerprint {
				e = ErrConflict
			}
		}
		s.Store.mu.Unlock()
		if e != nil {
			return nil, e
		}
	}
	if record.State != "prepared" || in.Mode == "result" {
		if record.State == "rejected" {
			return map[string]any{"taskRef": record.ID, "state": "failed", "summary": "本次请求在下发前被拒绝，未执行；请检查原错误后重新发起。"}, nil
		}
		if record.State != "prepared" && record.CallID == "" {
			// Recover a receipt persisted before a process/network interruption,
			// without dispatching the operation again.
			var saved Call
			if s.Store.db.Table(s.Store.callTable).Where("call_key = ? AND source = ?", digest(p.ID+"\x00"+input.RequestID), p.ID).First(&saved).Error == nil {
				record.CallID = saved.ID
			}
		}
		if record.CallID != "" {
			call, e := s.Store.call(record.CallID)
			if e != nil || call.Source != p.ID {
				return nil, ErrDenied
			}
			return s.genericReceipt(ctx, p, in.Actor, record.ID, call, in.Mode == "result")
		}
		if record.State != "prepared" {
			return map[string]any{"taskRef": record.ID, "state": "uncertain", "summary": "请求已保留，执行结果尚未确认。请读取原回执，不要重新提交。"}, nil
		}
	}
	if time.Since(record.CreatedAt) > 5*time.Minute {
		return nil, fmt.Errorf("%w: confirmation expired", ErrConflict)
	}
	if record.Revision != revision {
		return nil, fmt.Errorf("%w: operation changed", ErrConflict)
	}
	if in.Mode == "result" || in.Mode == "run" && policy.Confirmation != "none" {
		return map[string]any{"taskRef": record.ID, "state": "needs_confirmation", "title": op.Semantics().Purpose, "arguments": json.RawMessage(op.RedactInput(raw)), "confirmation": policy.Confirmation, "expiresAt": record.CreatedAt.Add(5 * time.Minute)}, nil
	}
	reserved := s.Store.db.Table(s.Store.genericTable()).Where("id = ? AND state = ?", record.ID, "prepared").Update("state", "submitted")
	if reserved.Error != nil {
		return nil, reserved.Error
	}
	if reserved.RowsAffected != 1 {
		return map[string]any{"taskRef": record.ID, "state": "uncertain", "summary": "请求正在处理，请读取原结果。"}, nil
	}
	// Explicit execute comes only from the plugin's human-confirmation handler.
	// Ordinary model submissions still retain the existing model channel guards.
	if in.Mode != "execute" {
		ctx = WithModelChannel(ctx)
	}
	result, err := s.Invoke(ctx, p, op.ToolName(), raw)
	if err != nil && result.CallID == "" {
		// invokeAPI returns a call identity once dispatch is possible. An
		// admission/validation rejection is distinct from a lost execution.
		if e := s.Store.db.Table(s.Store.genericTable()).Where("id = ?", record.ID).Update("state", "rejected").Error; e != nil {
			return nil, ErrUncertain
		}
	}
	if result.CallID != "" {
		if e := s.Store.db.Table(s.Store.genericTable()).Where("id = ?", record.ID).Update("call_id", result.CallID).Error; e != nil {
			return nil, ErrUncertain
		}
	}
	if err != nil {
		return genericResult(record.ID, result), err
	}
	call, e := s.Store.call(result.CallID)
	if e != nil {
		return nil, e
	}
	return s.genericReceipt(ctx, p, in.Actor, record.ID, call, false)
}

func (s *Service) genericReceipt(ctx context.Context, p Principal, a CompanionActor, ref string, call Call, refresh bool) (map[string]any, error) {
	out := genericResult(ref, invocation(call))
	op, ok := s.catalog.Operation(call.Tool)
	if !ok {
		return out, nil
	}
	applyTaskState(out, op, json.RawMessage(call.Result))
	var input apicontract.Input
	_ = json.Unmarshal([]byte(call.Arguments), &input)
	next := []apicontract.ResultRead{}
	for _, read := range op.ResultReads(json.RawMessage(call.Result), input) {
		target, exists := s.catalog.Operation(read.OperationID)
		if exists && s.genericAllowed(p, a, target) {
			next = append(next, read)
		}
	}
	if len(next) > 0 {
		out["next"] = next
	}
	if refresh && call.Status == "succeeded" && len(next) > 0 {
		raw, _ := json.Marshal(next[0].Arguments)
		latest, err := s.Invoke(WithModelChannel(ctx), p, apicontract.ToolPrefix+next[0].OperationID, raw)
		if err != nil {
			return nil, err
		}
		var latestData any
		_ = json.Unmarshal(latest.Data, &latestData)
		for path, expected := range next[0].Match {
			actual, _ := json.Marshal(apicontract.ResultValue(latestData, path))
			wanted, _ := json.Marshal(expected)
			if string(actual) != string(wanted) {
				out["progressUnavailable"] = "当前查询已指向另一项任务，保留原操作回执。"
				return out, nil
			}
		}
		out["result"], out["resultCallId"] = latest.Data, latest.CallID
		out["state"] = latest.Status
		target, _ := s.catalog.Operation(next[0].OperationID)
		applyTaskState(out, target, latest.Data)
		out["summary"] = "已读取原任务最新状态；是否完成以业务 status、stage、outcome 和效果证据为准。"
	}
	return out, nil
}

func applyTaskState(out map[string]any, op *apicontract.Operation, data json.RawMessage) {
	if op == nil {
		return
	}
	if state := op.TaskState(data); state != "" {
		out["taskState"] = state
		if slices.Contains([]string{"queued", "pending", "running", "succeeded", "failed", "canceled", "partial", "uncertain", "prepared", "interrupted", "rolled_back", "superseded", "unavailable", "timed_out"}, state) {
			out["state"] = state
		} else if state == "sent" {
			out["state"] = "accepted"
		} else if state == "unresponsive" || state == "unknown" {
			out["state"] = "uncertain"
		} else {
			out["state"] = "running"
		}
	}
	if outcome, declared := op.TaskOutcome(data); declared {
		out["taskOutcome"] = outcome
		if out["state"] == "succeeded" {
			switch outcome {
			case "full":
			case "partial":
				out["state"] = "partial"
			case "none":
				out["state"] = "failed"
			default:
				out["state"] = "uncertain"
			}
		}
	}
}

func cloneGenericSchema(in map[string]any) map[string]any {
	b, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
func genericResult(ref string, r Invocation) map[string]any {
	if ref == "" {
		ref = r.CallID
	}
	state := r.Status
	var envelope map[string]any
	_ = json.Unmarshal(r.Data, &envelope)
	if r.Status == "succeeded" && envelope["httpStatus"] == float64(202) {
		state = "accepted"
	}
	return map[string]any{"taskRef": ref, "callId": r.CallID, "state": state, "result": r.Data, "summary": "以原业务结果为准；提交成功不代表游戏效果已确认。"}
}
