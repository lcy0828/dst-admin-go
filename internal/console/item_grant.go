package console

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/entitycatalog"
)

// ItemGrant exposes only a fixed item operation and its correlated receipt.
// It deliberately excludes scripts, custom commands and unrelated console output.
type ItemGrant struct {
	RunID            string      `json:"runId"`
	RoomID           string      `json:"roomId"`
	WorldID          string      `json:"worldId"`
	PlayerID         string      `json:"playerId"`
	Prefab           string      `json:"prefab"`
	Name             string      `json:"name"`
	Requested        int         `json:"requested"`
	Granted          *int        `json:"granted,omitempty"`
	Status           string      `json:"status"`
	Complete         bool        `json:"complete"`
	ExecutionOutcome string      `json:"executionOutcome"`
	MayHaveExecuted  bool        `json:"mayHaveExecuted"`
	Message          string      `json:"message"`
	ObservedAt       *time.Time  `json:"observedAt,omitempty"`
	SpawnAudit       *SpawnAudit `json:"spawnAudit,omitempty"`
}

// ResolveGrantItem uses code-owned vanilla metadata, never editable command
// definitions or caller-supplied classification. Animals/structures are not items.
func ResolveGrantItem(name string, count int) (entitycatalog.Entity, error) {
	if count < 1 || count > 40 || len(name) > 256 || utf8.RuneCountInString(name) > 100 || name == "" || strings.TrimSpace(name) != name {
		return entitycatalog.Entity{}, ErrInvalidArguments
	}
	var match entitycatalog.Entity
	for _, item := range entitycatalog.Directory().Items {
		if item.Type != "item" && item.Type != "equipment" && item.Type != "food" {
			continue
		}
		if name != item.ID && name != item.NameZhCN && !strings.EqualFold(name, item.NameEn) {
			continue
		}
		if match.ID != "" && match.ID != item.ID {
			return entitycatalog.Entity{}, ErrInvalidArguments
		}
		match = item
	}
	if !prefabPattern.MatchString(match.ID) {
		return entitycatalog.Entity{}, ErrInvalidArguments
	}
	return match, nil
}

// GiveItem accepts only the fixed built-in recipe. Location must be confirmed by
// the caller before entry; a missing player at execution is never retried elsewhere.
func (s *Service) GiveItem(ctx context.Context, room, world, player, item string, count int) (ItemGrant, error) {
	resolved, err := ResolveGrantItem(item, count)
	if err != nil || !dstUserIDPattern.MatchString(player) {
		return ItemGrant{}, ErrInvalidArguments
	}
	r, err := s.rooms.Room(room)
	if err != nil {
		return ItemGrant{}, err
	}
	run, err := s.Execute(ctx, room, world, ExecuteRequest{CommandID: "give_item", Confirmation: r.Name, Arguments: map[string]interface{}{
		"player_id": player, "prefab": resolved.ID, "count": count, "quantity_mode": "units",
	}})
	if err != nil {
		return ItemGrant{}, err
	}
	return projectItemGrant(run), nil
}

func (s *Service) ItemGrant(ctx context.Context, room, player, runID string, refresh bool) (ItemGrant, error) {
	run, err := s.Run(runID)
	if err != nil {
		return ItemGrant{}, err
	}
	if run.RoomID != room || run.Mode != "builtin" || run.CommandID != "give_item" || run.Arguments["player_id"] != player || run.Arguments["quantity_mode"] != "units" {
		return ItemGrant{}, ErrRunNotFound
	}
	prefab, _ := run.Arguments["prefab"].(string)
	count, err := intArgument(run.Arguments, "count", 1, 40)
	if err != nil {
		return ItemGrant{}, ErrRunNotFound
	}
	if _, err = ResolveGrantItem(prefab, count); err != nil {
		return ItemGrant{}, ErrRunNotFound
	}
	if refresh {
		run, err = s.RefreshRun(ctx, room, runID)
		if err != nil {
			return ItemGrant{}, err
		}
	}
	return projectItemGrant(run), nil
}

func projectItemGrant(run Run) ItemGrant {
	player, _ := run.Arguments["player_id"].(string)
	prefab, _ := run.Arguments["prefab"].(string)
	count, _ := intArgument(run.Arguments, "count", 1, 40)
	item, _ := ResolveGrantItem(prefab, count)
	r := ItemGrant{RunID: run.ID, RoomID: run.RoomID, WorldID: run.WorldID, PlayerID: player, Prefab: prefab, Name: item.NameZhCN, Requested: count,
		Status: "uncertain", ExecutionOutcome: "unconfirmed", MayHaveExecuted: true, Message: "发放结果未确认；只读取原回执，不要重新发放", ObservedAt: run.ObservedAt, SpawnAudit: run.SpawnAudit}
	if run.Status == RunSending {
		r.Status = "running"
	}
	// A Lua exception can follow partial side effects. Never claim zero delivery.
	if run.Status == RunFailed {
		r.Status = "failed"
		r.Complete = true
		r.Message = "执行失败，可能已部分发放；不要自动重试"
		return r
	}
	if run.Status != RunSucceeded || run.ExecutionOutcome != "confirmed" || run.Output == nil || run.Output.Truncated || len(run.Output.Text) > 16<<10 {
		return r
	}
	var summary []string
	for _, line := range strings.Split(run.Output.Text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "[DST-ADMIN-GIVE]" {
			continue
		}
		if summary != nil {
			return r
		}
		summary = fields
	}
	if len(summary) == 3 && summary[1] == "PLAYER_NOT_FOUND" && summary[2] == player {
		zero := 0
		r.Granted = &zero
		r.Status = "failed"
		r.Complete = true
		r.ExecutionOutcome = "rejected"
		r.MayHaveExecuted = false
		r.Message = "执行时玩家已不在该分片，未发放；没有换分片重发"
		return r
	}
	if len(summary) != 5 || summary[1] != player || summary[2] != prefab || summary[3] != "units" {
		return r
	}
	n, err := strconv.Atoi(summary[4])
	if err != nil || n != count {
		return r
	}
	r.Granted = &n
	r.Status = "succeeded"
	r.Complete = true
	r.ExecutionOutcome = "confirmed"
	r.Message = "已生成并发放请求数量的物品；背包满时可能落地，堆叠合并后的去向以回执审计为准"
	return r
}
