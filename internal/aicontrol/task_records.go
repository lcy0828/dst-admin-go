package aicontrol

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

func (s *Service) prepareTask(p Principal, in BotInput, write bool, player ...string) (taskRecord, error) {
	now := time.Now().UTC()
	raw, _ := json.Marshal(in)
	record := taskRecord{ReadOnly: !write, ID: uuid.NewString(), Source: p.ID, RoomID: in.RoomID, Input: string(raw), State: "prepared", CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	if len(player) == 1 {
		record.PlayerID = player[0]
	}
	if !write {
		record.State = "submitted"
	}
	s.Store.mu.Lock()
	defer s.Store.mu.Unlock()
	// Expired task references are rejected, never turned into new executions.
	if e := s.Store.db.Table(s.Store.taskTable).Where("created_at < ?", now.Add(-30*24*time.Hour)).Delete(&taskRecord{}).Error; e != nil {
		return record, e
	}
	// Read handles are short-lived; high-frequency queries must not consume the
	// write replay budget. Deletion never permits execution of an old handle.
	if e := s.Store.db.Table(s.Store.taskTable).Where("read_only = ? AND created_at < ?", true, now.Add(-24*time.Hour)).Delete(&taskRecord{}).Error; e != nil {
		return record, e
	}
	var reads int
	if e := s.Store.db.Table(s.Store.taskTable).Where("read_only = ?", true).Count(&reads).Error; e != nil {
		return record, e
	}
	if reads >= 2000 {
		var oldest []taskRecord
		if e := s.Store.db.Table(s.Store.taskTable).Where("read_only = ?", true).Order("created_at ASC").Limit(200).Find(&oldest).Error; e != nil {
			return record, e
		}
		ids := []string{}
		for _, r := range oldest {
			ids = append(ids, r.ID)
		}
		if e := s.Store.db.Table(s.Store.taskTable).Where("id IN (?) AND read_only = ?", ids, true).Delete(&taskRecord{}).Error; e != nil {
			return record, e
		}
	}
	var count int
	if e := s.Store.db.Table(s.Store.taskTable).Count(&count).Error; e != nil {
		return record, e
	}
	if count >= 10000 {
		return record, ErrBusy
	}
	return record, s.Store.db.Table(s.Store.taskTable).Create(&record).Error
}
func preparedTask(r taskRecord, in BotInput) TaskResponse {
	return TaskResponse{State: "prepared", Summary: "目标和参数已确定，尚未执行。使用此任务编号提交。", TaskRef: r.ID, RoomID: r.RoomID, CapabilityID: in.CapabilityID, Parameters: in.Parameters, ExpiresAt: &r.ExpiresAt}
}
func (s *Service) loadTask(p Principal, id string) (taskRecord, BotInput, error) {
	var r taskRecord
	var in BotInput
	if _, e := uuid.Parse(id); e != nil {
		return r, in, ErrInvalid
	}
	if e := s.Store.db.Table(s.Store.taskTable).Where("id = ? AND source = ?", id, p.ID).First(&r).Error; e != nil {
		return r, in, ErrDenied
	}
	if !p.AllowsRoom(r.RoomID) || json.Unmarshal([]byte(r.Input), &in) != nil {
		return r, in, ErrDenied
	}
	if r.State == "prepared" {
		if _, e := s.botCapability(p, in.CapabilityID, r.RoomID); e != nil {
			return r, in, e
		}
	} // Submitted receipts use BotResult's recorded-operation permission checks.
	return r, inputWithTask(in, r.ID), nil
}
func (s *Service) executeTask(ctx context.Context, p Principal, id string) (TaskResponse, error) {
	r, in, e := s.loadTask(p, id)
	if e != nil {
		return TaskResponse{}, e
	}
	if r.State != "prepared" {
		return s.taskResult(ctx, p, id)
	}
	if !time.Now().Before(r.ExpiresAt) {
		return TaskResponse{State: "expired", TaskRef: id, Summary: "该准备任务已过期，未提交。请重新准备。"}, nil
	}
	c, e := s.botCapability(p, in.CapabilityID, r.RoomID)
	if e != nil {
		return TaskResponse{}, e
	}
	if c.Access != "write" || c.SchemaRevision != in.SchemaRevision {
		return TaskResponse{}, ErrConflict
	}
	if e = validateTaskCapability(s, p, c, in); e != nil {
		return TaskResponse{}, e
	}
	// Reserve before dispatch. A crash between reservation and BotExecute is
	// uncertain, never grounds for automatically starting a second operation.
	change := s.Store.db.Table(s.Store.taskTable).Where("id = ? AND state = ?", id, "prepared").UpdateColumn("state", "submitted")
	if change.Error != nil {
		return TaskResponse{}, change.Error
	}
	if change.RowsAffected != 1 {
		return s.taskResult(ctx, p, id)
	}
	result, e := s.BotExecute(ctx, p, in, true)
	if e != nil {
		return TaskResponse{State: "uncertain", TaskRef: id, Summary: "提交结果未确认。请查询原任务，不要重新准备或重发。"}, nil
	}
	return projectTaskResponse(r, result), nil
}
func (s *Service) taskResult(ctx context.Context, p Principal, id string) (TaskResponse, error) {
	r, in, e := s.loadTask(p, id)
	if e != nil {
		return TaskResponse{}, e
	}
	if r.State == "prepared" {
		if !time.Now().Before(r.ExpiresAt) {
			return TaskResponse{State: "expired", TaskRef: id, Summary: "该准备任务已过期，未执行。"}, nil
		}
		return preparedTask(r, in), nil
	}
	result, e := s.BotResult(ctx, p, r.RoomID, in.RequestID)
	if e != nil {
		return TaskResponse{State: "uncertain", TaskRef: id, Summary: "原任务结果尚不可确认；没有重新执行。"}, nil
	}
	return projectTaskResponse(r, result), nil
}
func taskResponse(id string, r BotResult) TaskResponse {
	summary := "结果尚未确认，请读取原任务。"
	switch r.Status {
	case "succeeded":
		if r.Complete {
			summary = "已取得结果，请根据来源、采样时间和执行证据判断。"
		}
	case "pending", "running":
		summary = "任务正在处理，可使用同一任务编号查询进度。"
	case "failed", "rejected", "unavailable":
		summary = "本次请求未完成，请查看错误原因。"
	case "partial":
		summary = "已取得部分结果；缺失信息不能当作零或没有发生。"
	}
	return TaskResponse{State: r.Status, Summary: summary, TaskRef: id, RoomID: r.RoomID, CapabilityID: r.CapabilityID, Result: &r}
}

func projectTaskResponse(record taskRecord, result BotResult) TaskResponse {
	response := taskResponse(record.ID, result)
	if result.CapabilityID == "room.network" && record.PlayerID != "" {
		if data, ok := result.Data.(map[string]any); ok {
			filtered := []map[string]any{}
			for _, row := range taskRows(data) {
				if row["id"] == record.PlayerID {
					filtered = append(filtered, row)
				}
			}
			projected := map[string]any{}
			for k, v := range data {
				projected[k] = v
			}
			roomSummary := map[string]any{}
			for _, key := range []string{"online", "staleOnline", "total", "hasMore"} {
				if value, ok := projected[key]; ok {
					roomSummary[key] = value
					delete(projected, key)
				}
			}
			projected["roomSummary"] = roomSummary
			projected["playerId"] = record.PlayerID
			projected["items"] = filtered
			projected["units"] = "raw_scores"
			projected["note"] = "网络和客户端性能为原始档位，不能换算为毫秒。"
			result.Data = projected
			response.Result = &result
		}
	}
	return response
}
