package aicontrol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"dont/internal/apicontract"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

// Bot capabilities are code-owned recipes. Neither metadata nor callers can
// supply a handler, operation, script or alternate execution URL.
type BotCapability struct {
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	Category        string            `json:"category"`
	Access          string            `json:"access"`
	ContractVersion int               `json:"contractVersion"`
	SchemaRevision  string            `json:"schemaRevision"`
	Parameters      map[string]any    `json:"parameters"`
	ParameterHints  map[string]string `json:"parameterHints"`
	NativeTools     []string          `json:"nativeTools"`
	ResultKind      string            `json:"resultKind"`
	Operation       string            `json:"-"`
	build           func(string, map[string]any) apicontract.Input
}
type BotInput struct {
	Operation       string         `json:"originalOperation,omitempty"`
	RoomID          string         `json:"roomId"`
	CapabilityID    string         `json:"capabilityId"`
	ContractVersion int            `json:"contractVersion"`
	SchemaRevision  string         `json:"schemaRevision"`
	Parameters      map[string]any `json:"parameters"`
	RequestID       string         `json:"requestId"`
}
type BotResult struct {
	callID           string
	RequestID        string            `json:"requestId"`
	CapabilityID     string            `json:"capabilityId"`
	ContractVersion  int               `json:"contractVersion"`
	RoomID           string            `json:"roomId"`
	Status           string            `json:"status"`
	Complete         bool              `json:"complete"`
	ObservedAt       any               `json:"observedAt"`
	Source           string            `json:"source"`
	Cached           bool              `json:"cached"`
	ExecutionOutcome string            `json:"executionOutcome"`
	MayHaveExecuted  bool              `json:"mayHaveExecuted"`
	ResultKind       string            `json:"resultKind"`
	Data             any               `json:"data"`
	JobID            string            `json:"jobId,omitempty"`
	QueryRunID       string            `json:"queryRunId,omitempty"`
	Error            map[string]string `json:"error,omitempty"`
	NextAction       string            `json:"nextAction"`
}

var botID = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
var botRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

func botString(max int) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": max}
}
func botSchema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
func BotCapabilities() []BotCapability {
	empty := func(room string, _ map[string]any) apicontract.Input {
		return apicontract.Input{Path: map[string]any{"roomId": room}}
	}
	player := func(room string, p map[string]any) apicontract.Input {
		return apicontract.Input{Path: map[string]any{"roomId": room, "playerId": p["playerId"]}}
	}
	pSchema := botSchema(map[string]any{"playerId": botString(128)}, "playerId")
	list := []BotCapability{
		{ID: "room.status", Title: "房间状态", Category: "status", Operation: "getRoomRuntimeOverview", build: empty},
		{ID: "room.players", Title: "在线玩家", Category: "players", Operation: "listRoomPlayers", build: func(r string, _ map[string]any) apicontract.Input {
			v := empty(r, nil)
			v.Query = map[string]any{"status": "online", "includeAccessLists": false, "limit": 100}
			return v
		}},
		{ID: "room.network", Title: "网络档位", Category: "network", Operation: "listRoomPlayers", build: func(r string, _ map[string]any) apicontract.Input {
			v := empty(r, nil)
			v.Query = map[string]any{"status": "online", "includeAccessLists": false, "limit": 100}
			return v
		}},
		{ID: "player.resolve", Title: "查找玩家", Category: "profile", Operation: "resolveRoomPlayer", Parameters: botSchema(map[string]any{"query": botString(128)}, "query"), build: func(r string, p map[string]any) apicontract.Input {
			v := empty(r, nil)
			v.Query = map[string]any{"query": p["query"]}
			return v
		}},
		{ID: "player.inventory", Title: "玩家背包", Category: "inventory", Operation: "getRoomPlayerInventory", Parameters: pSchema, ParameterHints: map[string]string{"playerId": "player"}, build: func(r string, p map[string]any) apicontract.Input {
			v := player(r, p)
			v.Query = map[string]any{"source": "auto", "refresh": false}
			return v
		}},
		{ID: "room.mods", Title: "模组列表", Category: "mods", Operation: "listRoomMods", build: empty},
		{ID: "room.performance", Title: "性能报表", Category: "performance", Operation: "getRoomPerformanceReport", build: func(r string, _ map[string]any) apicontract.Input {
			v := empty(r, nil)
			v.Query = map[string]any{"maxPoints": 60}
			return v
		}},
		{ID: "room.backups.settings", Title: "自动备份设置", Category: "backups", Operation: "listRoomBackupSchedules", build: empty},
		{ID: "room.backups.configure", Title: "设置自动备份", Category: "management", Access: "write", Operation: "configureRoomBackupSchedule", Parameters: botSchema(map[string]any{"enabled": map[string]any{"type": "boolean"}, "schedule": botString(100), "timezone": botString(64), "keep": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "taskId": botString(128)}, "enabled"), build: func(r string, p map[string]any) apicontract.Input { v := empty(r, nil); v.Body = p; return v }},
		{ID: "room.backup.create", Title: "立即备份", Category: "management", Access: "write", Operation: "createRoomBackup", build: func(r string, _ map[string]any) apicontract.Input {
			v := empty(r, nil)
			v.Body = map[string]any{}
			return v
		}},
		{ID: "player.kick", Title: "踢出玩家", Category: "management", Access: "write", Operation: "kickRoomPlayer", Parameters: pSchema, ParameterHints: map[string]string{"playerId": "player"}, build: func(r string, p map[string]any) apicontract.Input {
			v := player(r, p)
			v.Body = map[string]any{}
			return v
		}},
	}
	for _, action := range []string{"start", "stop", "restart", "save"} {
		a := action
		titles := map[string]string{"start": "启动房间", "stop": "停止房间", "restart": "重启房间", "save": "保存世界"}
		list = append(list, BotCapability{ID: "room." + a, Title: titles[a], Category: "management", Access: "write", Operation: "runRoomAction", build: func(r string, _ map[string]any) apicontract.Input {
			v := empty(r, nil)
			v.Path["action"] = a
			v.Body = map[string]any{}
			return v
		}})
	}
	for _, q := range []string{"world_info", "online_players", "entity_count"} {
		query := q
		props := map[string]any{"worldId": botString(255)}
		required := []string{"worldId"}
		hints := map[string]string{"worldId": "world"}
		if query == "entity_count" {
			props["prefab"] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_]{1,80}$"}
			required = append(required, "prefab")
			hints["prefab"] = "resource"
		}
		list = append(list, BotCapability{ID: "world." + query, Title: map[string]string{"world_info": "世界信息", "online_players": "分片玩家", "entity_count": "实体计数"}[query], Category: "world", Operation: "queryRoomWorld", Parameters: botSchema(props, required...), ParameterHints: hints, build: func(r string, p map[string]any) apicontract.Input {
			v := empty(r, nil)
			v.Path["worldId"] = p["worldId"]
			v.Path["queryId"] = query
			if query == "entity_count" {
				v.Query = map[string]any{"prefab": p["prefab"]}
			}
			return v
		}})
	}
	for i := range list {
		c := &list[i]
		c.ContractVersion = 1
		if c.Access == "" {
			c.Access = "read"
		}
		if c.Parameters == nil {
			c.Parameters = botSchema(map[string]any{})
		}
		if c.ParameterHints == nil {
			c.ParameterHints = map[string]string{}
		}
		c.NativeTools = []string{"api_" + c.Operation}
		if c.ID == "room.status" {
			c.NativeTools = append(c.NativeTools, "api_listRoomWorlds", "api_listWorldStates")
		}
		c.ResultKind = "facts"
		if c.Access == "write" {
			c.ResultKind = "job"
		}
		if c.ID == "room.backups.configure" {
			c.ResultKind = "facts"
		}
		// Presentation changes do not invalidate a frozen parameter form.
		c.SchemaRevision = botSchemaRevision(*c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}
func botOperation(id string) bool {
	return id == "listBotCapabilities" || id == "queryBotCapability" || id == "runBotCapability" || id == "getBotCapabilityResult"
}
func (s *Service) botCapability(p Principal, id, room string) (BotCapability, error) {
	if s.catalog == nil || !p.AllowsRoom(room) {
		return BotCapability{}, ErrDenied
	}
	for _, c := range BotCapabilities() {
		if c.ID == id {
			if _, ok := s.catalog.Operation(c.Operation); !ok || !p.AllowsOperation(c.Operation, room) {
				return BotCapability{}, ErrDenied
			}
			return c, nil
		}
	}
	return BotCapability{}, ErrInvalid
}
func (s *Service) BotDirectory(p Principal, version, limit int, known, cursor string) (map[string]any, error) {
	if !s.Enabled() || p.ID == "" || version != 1 || limit < 1 || limit > 64 {
		return nil, ErrInvalid
	}
	items := []BotCapability{}
	for _, c := range BotCapabilities() {
		for _, r := range append(append([]string{}, p.RoomIDs...), p.RoomID, "__unscoped__") {
			if _, e := s.botCapability(p, c.ID, r); e == nil {
				items = append(items, c)
				break
			}
		}
	}
	encoded, _ := json.Marshal([]any{items, p.ID, p.Profile, p.RoomIDs, p.RoomID})
	revision := digest(string(encoded))
	offset := 0
	if len(cursor) > 512 {
		return nil, ErrInvalid
	}
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return nil, ErrInvalid
		}
		var v struct {
			Revision string
			Offset   int
		}
		if json.Unmarshal(raw, &v) != nil || v.Revision != revision || v.Offset < 0 || v.Offset > len(items) {
			return nil, ErrConflict
		}
		offset = v.Offset
	}
	if known == revision && cursor == "" {
		return map[string]any{"protocolVersion": 1, "revision": revision, "notModified": true}, nil
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	out := map[string]any{"protocolVersion": 1, "revision": revision, "items": items[offset:end], "total": len(items), "nextCursor": ""}
	if end < len(items) {
		b, _ := json.Marshal(map[string]any{"Revision": revision, "Offset": end})
		out["nextCursor"] = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}

// StrictBotJSON rejects duplicate keys, trailing documents and deep inputs.
func StrictBotJSON(raw []byte) (map[string]any, error)     { return strictBotJSONLimit(raw, 8192, 4) }
func StrictBotBodyJSON(raw []byte) (map[string]any, error) { return strictBotJSONLimit(raw, 12<<10, 5) }
func strictBotJSONLimit(raw []byte, maximum, maxDepth int) (map[string]any, error) {
	if len(raw) > maximum {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))

	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > maxDepth {
			return nil, ErrInvalid
		}
		t, e := d.Token()
		if e != nil {
			return nil, e
		}
		switch v := t.(type) {
		case json.Delim:
			if v == '{' {
				m := map[string]any{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return nil, e
					}
					key, ok := k.(string)
					if !ok {
						return nil, ErrInvalid
					}
					if _, ok = m[key]; ok {
						return nil, ErrInvalid
					}
					item, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					m[key] = item
				}
				_, e = d.Token()
				return m, e
			}
			if v == '[' {
				a := []any{}
				for d.More() {
					item, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, item)
				}
				_, e = d.Token()
				return a, e
			}
			return nil, ErrInvalid
		default:
			return t, nil
		}
	}
	v, e := read(0)
	if e != nil {
		return nil, ErrInvalid
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, ErrInvalid
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrInvalid
	}
	return m, nil
}
func (s *Service) botBackend(ctx context.Context, p Principal, c BotCapability, in apicontract.Input) (map[string]any, error) {
	op, ok := s.catalog.Operation(c.Operation)
	if !ok || !p.AllowsAPI(op, in) {
		return nil, ErrDenied
	}
	if (c.Access == "read") == op.Mutation() {
		return nil, ErrDenied
	}
	raw, _ := json.Marshal(in)
	validated, e := op.Validate(raw)
	if e != nil {
		return nil, e
	}
	backend, ok := s.backend.(APIBackend)
	if !ok {
		return nil, ErrUnavailable
	}
	raw, e = backend.InvokeAPI(ctx, p, op, validated)
	if e != nil {
		return nil, e
	}
	var envelope struct {
		HTTPStatus int            `json:"httpStatus"`
		Data       map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.HTTPStatus >= 400 || envelope.Data == nil {
		return nil, ErrUnavailable
	}
	return envelope.Data, nil
}
func (s *Service) BotExecute(ctx context.Context, p Principal, in BotInput, write bool) (BotResult, error) {
	if ctx.Err() != nil {
		return BotResult{}, ctx.Err()
	}
	if !s.Enabled() || p.ID == "" || !botID.MatchString(in.CapabilityID) || !botRequestID.MatchString(in.RequestID) || in.RoomID == "" {
		return BotResult{}, ErrInvalid
	}
	c, e := s.botCapability(p, in.CapabilityID, in.RoomID)
	if e != nil {
		return BotResult{}, e
	}
	if (c.Access == "write") != write {
		return BotResult{}, ErrDenied
	}
	if in.ContractVersion != 1 || in.SchemaRevision != c.SchemaRevision {
		return BotResult{}, ErrConflict
	}
	if in.Parameters == nil {
		in.Parameters = map[string]any{}
	}
	encoded, e := json.Marshal(in.Parameters)
	if e != nil {
		return BotResult{}, ErrInvalid
	}
	params, e := StrictBotJSON(encoded)
	if e != nil {
		return BotResult{}, e
	}
	schemaRaw, _ := json.Marshal(c.Parameters)
	var sch jsonschema.Schema
	if json.Unmarshal(schemaRaw, &sch) != nil {
		return BotResult{}, ErrInvalid
	}
	validator, e := sch.Resolve(nil)
	if e != nil || validator.Validate(params) != nil {
		return BotResult{}, ErrInvalid
	}
	target := c.build(in.RoomID, params)
	// Validate the real operation before reserving a write. Metadata cannot grant permission.
	op, _ := s.catalog.Operation(c.Operation)
	if !p.AllowsAPI(op, target) {
		return BotResult{}, ErrDenied
	}
	if op.Mutation() {
		target.RequestID = "bot-" + digest(p.ID+"\x00"+in.RequestID)
	}
	in.Operation = c.Operation
	rawTarget, _ := json.Marshal(target)
	if _, e = op.Validate(rawTarget); e != nil {
		return BotResult{}, fmt.Errorf("%w: capability parameters: %v", ErrInvalid, e)
	}
	encoded, _ = json.Marshal(in)
	fingerprint := digest(string(encoded))
	key := digest(p.ID + "\x00bot\x00" + in.RequestID)
	s.Store.mu.Lock()
	var old Call
	found := s.Store.db.Table(s.Store.callTable).Where("call_key = ?", key).First(&old)
	if found.Error == nil {
		s.Store.mu.Unlock()
		if old.Fingerprint != fingerprint {
			return BotResult{}, ErrConflict
		}
		return s.botReadCall(ctx, p, old, false)
	}
	if !found.RecordNotFound() {
		s.Store.mu.Unlock()
		return BotResult{}, found.Error
	}
	if e = s.Store.compactBotRecords(); e != nil {
		s.Store.mu.Unlock()
		return BotResult{}, e
	}
	var count int
	if e = s.Store.db.Table(s.Store.callTable).Where("tool IN (?)", []string{"bot.read", "bot.write"}).Count(&count).Error; e != nil || count >= 10000 {
		s.Store.mu.Unlock()
		return BotResult{}, ErrBusy
	}
	select {
	case s.toolSlots <- struct{}{}:
		defer func() { <-s.toolSlots }()
	default:
		s.Store.mu.Unlock()
		return BotResult{}, ErrBusy
	}
	call := Call{ID: uuid.NewString(), CallKey: key, Fingerprint: fingerprint, Source: p.ID, Tool: "bot.read", RoomID: in.RoomID, Arguments: string(encoded), Status: "executing", CreatedAt: time.Now().UTC()}
	if write {
		call.Tool = "bot.write"
	}
	e = s.Store.db.Table(s.Store.callTable).Create(&call).Error
	s.Store.mu.Unlock()
	if e != nil {
		return BotResult{}, e
	}
	result := BotResult{callID: call.ID, RequestID: in.RequestID, CapabilityID: c.ID, ContractVersion: 1, RoomID: in.RoomID, Status: "uncertain", Source: "platform", ResultKind: c.ResultKind, NextAction: "read_original", ExecutionOutcome: "unconfirmed", MayHaveExecuted: write}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, e := s.botBackend(ctx, p, c, target)
	if e != nil {
		result.Error = map[string]string{"code": "BOT_EXECUTION_UNCONFIRMED", "message": "请求结果尚未确认，请读取原请求记录"}
		if !write {
			result.Status = "unavailable"
			result.NextAction = "none"
			result.ExecutionOutcome = "not_required"
		}
	} else {
		result = botProject(result, c, data)
	}
	serialized, _ := json.Marshal(result)
	if len(serialized) > 64<<10 {
		result.Data = nil
		result.Complete = false
		result.Status = "partial"
		result.Error = map[string]string{"code": "BOT_RESULT_TOO_LARGE", "message": "结果超出展示预算，请缩小查询范围"}
		serialized, _ = json.Marshal(result)
	}
	status := "succeeded"
	if result.Status == "uncertain" {
		status = "uncertain"
	}
	e = s.Store.db.Table(s.Store.callTable).Where("id = ?", call.ID).Updates(map[string]any{"status": status, "result": string(serialized)}).Error
	if e != nil {
		return result, ErrUncertain
	}
	return result, nil
}
func botProject(r BotResult, c BotCapability, d map[string]any) BotResult {
	r.Status = "succeeded"
	r.Complete = true
	r.NextAction = "none"
	r.ExecutionOutcome = "not_required"
	r.MayHaveExecuted = false
	if c.ID == "room.backups.configure" {
		if _, ok := d["effectiveEnabled"].(bool); ok {
			r.MayHaveExecuted = true
			r.ExecutionOutcome = "confirmed"
			r.ResultKind = "facts"
			r.Data = botPick(d, "effectiveEnabled", "source", "created", "nextRunAt", "nextRunLocal", "task")
			return r
		}
	}
	if c.Access == "write" {
		r.MayHaveExecuted = true
		r.ExecutionOutcome = "unconfirmed"
		r.Complete = false
		r.Status = "uncertain"
		r.NextAction = "read_original"
		id, _ := d["id"].(string)
		status, _ := d["status"].(string)
		r.JobID = id
		if id != "" {
			r.Data = botPick(d, "id", "kind", "status", "outcome", "progress", "createdAt", "updatedAt", "error", "message")
			switch status {
			case "queued":
				r.Status = "pending"
			case "running", "canceling":
				r.Status = "running"
			case "failed", "canceled":
				r.Status = "failed"
				r.Complete = true
				r.NextAction = "none"
				r.ExecutionOutcome = "confirmed"
			case "succeeded":
				if d["outcome"] == "full" {
					r.Status = "succeeded"
					r.Complete = true
					r.NextAction = "none"
					r.ExecutionOutcome = "confirmed"
				} else if d["outcome"] == "partial" {
					r.Status = "partial"
					r.NextAction = "none"
				}
			}
		}
		return r
	}
	switch c.ID {
	case "room.players", "player.resolve":
		r.Data = botPick(d, "online", "staleOnline", "total", "status", "match", "coverage", "hasMore", "normalizationVersion")
		rows := []any{}
		if items, ok := d["items"].([]any); ok {
			for _, row := range items {
				if p, ok := row.(map[string]any); ok {
					rows = append(rows, botPick(p, "id", "name", "prefab", "online", "presenceStatus", "worldConfirmed", "worldId", "worldName", "lastSeenAt", "presenceObservedAt", "age"))
				}
			}
		}
		r.Data.(map[string]any)["items"] = rows
	case "room.network":
		r.Data = botPick(d, "online", "staleOnline", "total", "hasMore")
		rows := []any{}
		if items, ok := d["items"].([]any); ok {
			for _, v := range items {
				if item, ok := v.(map[string]any); ok {
					row := botPick(item, "id", "name", "netScore", "performance", "online", "presenceStatus")
					if fields, ok := item["fields"].(map[string]any); ok {
						row["fields"] = botPick(fields, "netScore", "performance")
					}
					rows = append(rows, row)
				}
			}
		}
		r.Data.(map[string]any)["items"] = rows
	case "room.status":
		r.Data = botPick(d, "roomId", "summary", "observedAt")
		rows := []any{}
		if shards, ok := d["shards"].([]any); ok {
			for _, v := range shards {
				if shard, ok := v.(map[string]any); ok {
					row := botPick(shard, "worldId", "worldName", "worldRole", "state", "runtime")
					if health, ok := shard["health"].(map[string]any); ok {
						row["health"] = botPick(health, "available", "freshness", "observedAt", "ageSeconds")
					}
					rows = append(rows, row)
				}
			}
		}
		r.Data.(map[string]any)["shards"] = rows
		r.ObservedAt = d["observedAt"]
	default:
		r.Data = d
	}
	if c.ID == "player.inventory" {
		if status, ok := d["status"].(string); ok && status == "partial" {
			r.Status = "partial"
			r.Complete = false
		}
		if v, ok := d["source"].(string); ok {
			r.Source = v
		}
		for _, key := range []string{"observedAt", "capturedAt", "snapshotAt", "savedAt"} {
			if d[key] != nil {
				r.ObservedAt = d[key]
				break
			}
		}
	}
	if d["hasMore"] == true {
		r.Status = "partial"
		r.Complete = false
	}
	if strings.HasPrefix(c.ID, "world.") {
		r.ResultKind = "query-run"
		if v, ok := d["status"].(string); ok {
			r.Status = v
		}
		r.Complete = d["complete"] == true
		r.Source, _ = d["source"].(string)
		r.ObservedAt = d["observedAt"]
		r.Cached = d["cached"] == true
		r.QueryRunID, _ = d["queryRunId"].(string)
		if r.Status == "queued" {
			r.Status = "pending"
		}
		if r.Status == "sent" || r.Status == "executing" {
			r.Status = "running"
		}
		if e, ok := d["error"].(map[string]any); ok {
			r.Error = map[string]string{}
			for _, k := range []string{"code", "message"} {
				if v, ok := e[k].(string); ok {
					r.Error[k] = v
				}
			}
		}
		if v, ok := d["executionOutcome"].(string); ok {
			r.ExecutionOutcome = v
		}
		r.MayHaveExecuted = d["mayHaveExecuted"] == true
		if !r.Complete {
			r.NextAction = "read_original"
		}
	}
	return r
}
func botPick(v map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if value, ok := v[k]; ok {
			out[k] = value
		}
	}
	return out
}
func (s *Service) BotResult(ctx context.Context, p Principal, room, request string) (BotResult, error) {
	if !s.Enabled() || !botRequestID.MatchString(request) || p.ID == "" || !p.AllowsRoom(room) {
		return BotResult{}, ErrDenied
	}
	select {
	case s.toolSlots <- struct{}{}:
		defer func() { <-s.toolSlots }()
	default:
		return BotResult{}, ErrBusy
	}
	var call Call
	e := s.Store.db.Table(s.Store.callTable).Where("call_key = ? AND source = ? AND room_id = ?", digest(p.ID+"\x00bot\x00"+request), p.ID, room).First(&call).Error
	if e != nil {
		if gorm.IsRecordNotFoundError(e) {
			return BotResult{}, ErrUnavailable
		}
		return BotResult{}, e
	}
	return s.botReadCall(ctx, p, call, true)
}
func (s *Service) botReadCall(ctx context.Context, p Principal, call Call, refresh bool) (BotResult, error) {
	var in BotInput
	if json.Unmarshal([]byte(call.Arguments), &in) != nil || call.Source != p.ID {
		return BotResult{}, ErrDenied
	}
	c, e := s.botCapability(p, in.CapabilityID, in.RoomID)
	if e != nil {
		// Removed capabilities can only expose their own existing result, under the
		// current permission for their recorded operation. They cannot execute again.
		if in.Operation == "" || !p.AllowsOperation(in.Operation, in.RoomID) {
			return BotResult{}, ErrDenied
		}
		c = BotCapability{ID: in.CapabilityID, Operation: in.Operation, Access: "read", ResultKind: "facts"}
		if call.Tool == "bot.write" {
			c.Access = "write"
			c.ResultKind = "job"
		}
	}
	r := BotResult{callID: call.ID, RequestID: in.RequestID, CapabilityID: in.CapabilityID, ContractVersion: 1, RoomID: in.RoomID, Status: "uncertain", NextAction: "read_original", ExecutionOutcome: "unconfirmed", MayHaveExecuted: call.Tool == "bot.write", ResultKind: c.ResultKind}
	if call.Result != "" {
		var stored BotResult
		if json.Unmarshal([]byte(call.Result), &stored) == nil && stored.RequestID == in.RequestID {
			r = stored
			r.callID = call.ID
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if refresh && r.JobID != "" && p.AllowsOperation("getJob", in.RoomID) {
		op, ok := s.catalog.Operation("getJob")
		if !ok {
			return r, nil
		}
		b, ok := s.backend.(APIBackend)
		if !ok {
			return r, nil
		}
		raw, e := b.InvokeAPI(ctx, p, op, apicontract.Input{Path: map[string]any{"jobId": r.JobID}})
		if e == nil {
			var envelope struct {
				Data map[string]any `json:"data"`
			}
			if json.Unmarshal(raw, &envelope) == nil && envelope.Data["roomId"] == in.RoomID {
				r = botProject(r, c, envelope.Data)
				v, _ := json.Marshal(r)
				if e := s.Store.db.Table(s.Store.callTable).Where("id = ?", call.ID).Updates(map[string]any{"result": string(v), "status": botStoredStatus(r)}).Error; e != nil {
					return r, e
				}
			}
		}
	}
	if refresh && r.QueryRunID != "" && strings.HasPrefix(c.ID, "world.") && p.AllowsOperation("getRoomWorldQueryRun", in.RoomID) {
		if op, ok := s.catalog.Operation("getRoomWorldQueryRun"); ok {
			if b, ok := s.backend.(APIBackend); ok {
				input := apicontract.Input{Path: map[string]any{"roomId": in.RoomID, "worldId": in.Parameters["worldId"], "runId": r.QueryRunID}, Query: map[string]any{"refresh": true}}
				raw, e := b.InvokeAPI(ctx, p, op, input)
				if e == nil {
					var env struct {
						Data map[string]any `json:"data"`
					}
					if json.Unmarshal(raw, &env) == nil && env.Data["roomId"] == in.RoomID {
						r = botProject(r, c, env.Data)
						v, _ := json.Marshal(r)
						if e = s.Store.db.Table(s.Store.callTable).Where("id = ?", call.ID).Updates(map[string]any{"result": string(v), "status": botStoredStatus(r)}).Error; e != nil {
							return r, e
						}
					}
				}
			}
		}
	}
	return r, nil
}

// BotAPI is shared by MCP and REST, so direct calls receive identical guards.
func (s *Service) BotAPI(ctx context.Context, p Principal, id string, in apicontract.Input) (any, error) {
	str := func(v any) string { s, _ := v.(string); return s }
	num := func(v any) int {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case json.Number:
			i, _ := n.Int64()
			return int(i)
		}
		return 0
	}
	switch id {
	case "listBotCapabilities":
		limit := num(in.Query["limit"])
		if limit == 0 {
			limit = 32
		}
		return s.BotDirectory(p, num(in.Query["protocolVersion"]), limit, str(in.Query["knownRevision"]), str(in.Query["cursor"]))
	case "getBotCapabilityResult":
		return s.BotResult(ctx, p, str(in.Path["roomId"]), str(in.Path["requestId"]))
	case "queryBotCapability", "runBotCapability":
		v := BotInput{RoomID: str(in.Path["roomId"]), CapabilityID: str(in.Path["capabilityId"])}
		if id == "queryBotCapability" {
			v.ContractVersion = num(in.Query["contractVersion"])
			v.SchemaRevision = str(in.Query["schemaRevision"])
			v.RequestID = str(in.Query["requestId"])
			raw := str(in.Query["parameters"])
			if raw == "" {
				raw = "{}"
			}
			p, e := StrictBotJSON([]byte(raw))
			if e != nil {
				return nil, e
			}
			v.Parameters = p
		} else {
			body, ok := in.Body.(map[string]any)
			if !ok {
				return nil, ErrInvalid
			}
			v.ContractVersion = num(body["contractVersion"])
			v.SchemaRevision = str(body["schemaRevision"])
			v.Parameters, _ = body["parameters"].(map[string]any)
			v.RequestID = in.RequestID
		}
		return s.BotExecute(ctx, p, v, id == "runBotCapability")
	}
	return nil, ErrInvalid
}
func (s *Service) invokeBotAPI(ctx context.Context, p Principal, op *apicontract.Operation, in apicontract.Input) (Invocation, error) {
	value, e := s.BotAPI(ctx, p, op.ID, in)
	if e != nil {
		return Invocation{}, e
	}
	raw, e := json.Marshal(map[string]any{"httpStatus": 200, "operationId": op.ID, "data": value})
	callID := uuid.NewString()
	if result, ok := value.(BotResult); ok && result.callID != "" {
		callID = result.callID
	}
	return Invocation{CallID: callID, Status: "succeeded", Data: raw}, e
}

func botStoredStatus(result BotResult) string {
	if result.Status == "uncertain" {
		return "uncertain"
	}
	return "succeeded"
}

func botSchemaRevision(c BotCapability) string {
	encoded, _ := json.Marshal([]any{c.ID, c.Access, c.Category, c.Parameters, c.ParameterHints, c.Operation, c.NativeTools, c.ResultKind, c.ContractVersion})
	return digest(string(encoded))
}
