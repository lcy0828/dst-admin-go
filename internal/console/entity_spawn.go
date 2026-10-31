package console

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dont/internal/entitycatalog"
)

// SpawnEntityResult exposes only the fixed spawn action and its own evidence.
type SpawnEntityResult struct {
	RunID            string      `json:"runId"`
	RoomID           string      `json:"roomId"`
	WorldID          string      `json:"worldId"`
	PlayerID         string      `json:"playerId"`
	Prefab           string      `json:"prefab"`
	Name             string      `json:"name"`
	Requested        int         `json:"requested"`
	Spawned          *int        `json:"spawned,omitempty"`
	Status           string      `json:"status"`
	Complete         bool        `json:"complete"`
	ExecutionOutcome string      `json:"executionOutcome"`
	MayHaveExecuted  bool        `json:"mayHaveExecuted"`
	Message          string      `json:"message"`
	ObservedAt       *time.Time  `json:"observedAt,omitempty"`
	SpawnAudit       *SpawnAudit `json:"spawnAudit,omitempty"`
}

// Explicitly reviewed ordinary creatures only. Directory category or caller
// metadata cannot authorize constructors that require special world setup.
var controlledSpawnPrefabs = map[string]bool{"warg": true, "hound": true, "spider": true, "pigman": true, "rabbit": true, "bee": true}

func ResolveSpawnEntity(name string, count int) (entitycatalog.Entity, error) {
	if count < 1 || count > 5 || len(name) > 100 || name == "" || strings.TrimSpace(name) != name {
		return entitycatalog.Entity{}, ErrInvalidArguments
	}
	if name == "狗王" {
		name = "warg"
	}
	var match entitycatalog.Entity
	for _, item := range entitycatalog.Directory().Items {
		if !controlledSpawnPrefabs[item.ID] || (name != item.ID && name != item.NameZhCN && !strings.EqualFold(name, item.NameEn)) {
			continue
		}
		if match.ID != "" && match.ID != item.ID {
			return entitycatalog.Entity{}, ErrInvalidArguments
		}
		match = item
	}
	if match.ID == "" {
		return entitycatalog.Entity{}, ErrInvalidArguments
	}
	return match, nil
}

func spawnPlayerEntityTemplate() template {
	min, max := 1, 5
	return template{definition: Definition{ID: "spawn_player_entity", Name: "受控生成玩家附近生物", Description: "仅生成固定白名单生物，确认玩家与陆地位置，单次最多5只", Category: "世界控制", Risk: RiskCritical, IsBuiltin: true,
		Script: "仅在当前分片在线玩家附近的可通行陆地生成受控生物，并返回本次数量和位置审计",
		Parameters: []Parameter{
			{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
			{Name: "prefab", Label: "生物 Prefab", Type: "string", Required: true, Options: []string{"warg", "hound", "spider", "pigman", "rabbit", "bee"}},
			{Name: "count", Label: "数量", Type: "integer", Required: true, Minimum: &min, Maximum: &max, Default: 1},
		}},
		render: func(args map[string]interface{}) (string, error) {
			player, err := matchedArgument(args, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			prefab, err := matchedArgument(args, "prefab", prefabPattern)
			if err != nil {
				return "", err
			}
			count, err := intArgument(args, "count", 1, 5)
			if err != nil {
				return "", err
			}
			if _, err = ResolveSpawnEntity(prefab, count); err != nil {
				return "", err
			}
			// Preflight at most 40 map points before creating anything. No global
			// entity scan, deferred task, automatic retry or world save.
			return withSpawnAudit(fmt.Sprintf(`local uid=%s;local prefab=%s;local requested=%d;local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==uid then p=v;break end end;local function result(n,status)print("[DST-ADMIN-CONTROLLED-SPAWN]",uid,prefab,requested,n,status)end;if not p or not p.Transform or p.migration or (p.HasTag and p:HasTag("playerghost")) or (p.components and p.components.health and p.components.health:IsDead())then result(0,"PLAYER_UNAVAILABLE");return end;local map=TheWorld and TheWorld.Map;if not map or type(map.IsPassableAtPoint)~="function"or type(map.IsOceanAtPoint)~="function"then result(0,"MAP_UNAVAILABLE");return end;local x,y,z=p.Transform:GetWorldPosition();local points={};for i=1,requested do local point=nil;for attempt=1,8 do local a=(i-1)*6.28318530718/requested+(attempt-1)*.31;local radius=5+attempt;local px,pz=x+math.cos(a)*radius,z+math.sin(a)*radius;if map:IsPassableAtPoint(px,0,pz)and not map:IsOceanAtPoint(px,0,pz)then point={px,y,pz};break end end;if not point then result(0,"NO_SAFE_POSITION");return end;points[i]=point end;local n=0;for _,point in ipairs(points)do local ok,e=pcall(SpawnPrefab,prefab);if not ok or not e or not e.Transform then result(n,"SPAWN_FAILED");return end;e.Transform:SetPosition(point[1],point[2],point[3]);__dst_record(e,p,"world",1);n=n+1 end;result(n,"OK")`, quoteLua(player), quoteLua(prefab), count)), nil
		}}
}

func (s *Service) SpawnPlayerEntity(ctx context.Context, room, world, player, name string, count int) (SpawnEntityResult, error) {
	entity, err := ResolveSpawnEntity(name, count)
	if err != nil || !dstUserIDPattern.MatchString(player) {
		return SpawnEntityResult{}, ErrInvalidArguments
	}
	r, err := s.rooms.Room(room)
	if err != nil {
		return SpawnEntityResult{}, err
	}
	run, err := s.Execute(ctx, room, world, ExecuteRequest{CommandID: "spawn_player_entity", Confirmation: r.Name, Arguments: map[string]interface{}{"player_id": player, "prefab": entity.ID, "count": count}})
	if err != nil {
		return SpawnEntityResult{}, err
	}
	return projectEntitySpawn(run), nil
}

func (s *Service) EntitySpawn(ctx context.Context, room, player, runID string, refresh bool) (SpawnEntityResult, error) {
	run, err := s.Run(runID)
	if err != nil {
		return SpawnEntityResult{}, err
	}
	prefab, _ := run.Arguments["prefab"].(string)
	count, countErr := intArgument(run.Arguments, "count", 1, 5)
	if run.RoomID != room || run.Mode != "builtin" || run.CommandID != "spawn_player_entity" || run.Arguments["player_id"] != player || countErr != nil {
		return SpawnEntityResult{}, ErrRunNotFound
	}
	if _, err = ResolveSpawnEntity(prefab, count); err != nil {
		return SpawnEntityResult{}, ErrRunNotFound
	}
	if refresh {
		run, err = s.RefreshRun(ctx, room, runID)
		if err != nil {
			return SpawnEntityResult{}, err
		}
	}
	return projectEntitySpawn(run), nil
}

func projectEntitySpawn(run Run) SpawnEntityResult {
	player, _ := run.Arguments["player_id"].(string)
	prefab, _ := run.Arguments["prefab"].(string)
	count, _ := intArgument(run.Arguments, "count", 1, 5)
	entity, _ := ResolveSpawnEntity(prefab, count)
	r := SpawnEntityResult{RunID: run.ID, RoomID: run.RoomID, WorldID: run.WorldID, PlayerID: player, Prefab: prefab, Name: entity.NameZhCN, Requested: count, Status: "uncertain", ExecutionOutcome: "unconfirmed", MayHaveExecuted: true, Message: "生成结果未确认；只补读原回执，不要重复生成", ObservedAt: run.ObservedAt, SpawnAudit: run.SpawnAudit}
	if run.Status == RunSending {
		r.Status = "running"
	}
	if run.Status == RunFailed {
		r.Status = "failed"
		r.Complete = true
		r.Message = "执行失败，可能已有部分生成；不要自动重试"
		return r
	}
	if run.Status != RunSucceeded || run.ExecutionOutcome != "confirmed" || run.Output == nil || run.Output.Truncated || len(run.Output.Text) > 16<<10 {
		return r
	}
	var summary []string
	for _, line := range strings.Split(run.Output.Text, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "[DST-ADMIN-CONTROLLED-SPAWN]" {
			if summary != nil {
				return r
			}
			summary = fields
		}
	}
	if len(summary) != 6 || summary[1] != player || summary[2] != prefab || summary[3] != strconv.Itoa(count) {
		return r
	}
	n, err := strconv.Atoi(summary[4])
	if err != nil || n < 0 || n > count {
		return r
	}
	status := summary[5]
	if status == "OK" && n == count {
		r.Spawned = &n
		r.Status = "succeeded"
		r.Complete = true
		r.ExecutionOutcome = "confirmed"
		r.Message = "本次回执确认生成，坐标以本次生成审计为准"
		return r
	}
	if n == 0 && (status == "PLAYER_UNAVAILABLE" || status == "MAP_UNAVAILABLE" || status == "NO_SAFE_POSITION") {
		r.Spawned = &n
		r.Status = "failed"
		r.Complete = true
		r.ExecutionOutcome = "rejected"
		r.MayHaveExecuted = false
		r.Message = "玩家或附近陆地位置不可用，未生成；没有换分片重试"
		return r
	}
	if status == "SPAWN_FAILED" {
		r.Spawned = &n
		r.Status = "failed"
		r.Complete = true
		r.Message = "生成失败，可能有部分副作用；不要自动重试"
	}
	return r
}
