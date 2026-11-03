package console

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"dont/internal/dstruntime"
)

const receiptRefreshWindow = 2 * time.Minute

type receiptReader interface {
	ReadCommandReceipt(context.Context, string, string, dstruntime.CommandRequest, time.Time) (dstruntime.CommandReceipt, error)
}

// ResultState describes evidence, not arbitrary Lua's business meaning. Only a
// script's printed 0 can establish a zero count; missing output cannot.
func describeResult(run Run) Run {
	run.Movement = movementResult(run)
	if movementCommand(run.CommandID) && run.Mode == "builtin" && strings.Contains(run.Script, "w.MovePlayers(") && run.Status == RunSucceeded {
		if run.Movement == nil || run.Movement.State == "partial" {
			run.Status = RunUncertain
			run.MayHaveExecuted = true
			run.Message = "部分玩家的位置尚未确认，请查看逐人结果和原回执，不要重复提交。"
		} else if run.Movement.State == "confirmed" {
			run.Message = "传送已完成，玩家位置已确认。"
		} else if run.Movement.State == "preview" {
			run.Message = "已检查并固定本次玩家和落点，请在 60 秒内确认。尚未传送。"
		} else {
			run.Message = "已取消本次传送确认。"
		}
	}
	run.Batch = batchResult(run)
	if run.Mode == "builtin" && run.CommandID == "act_nearby_entities" && strings.Contains(run.Script, "w.Nearby(") && run.Status == RunSucceeded && run.Batch == nil {
		run.Status = RunUncertain
		run.MayHaveExecuted = true
		run.Message = "缺少有效的范围操作结果，请查看原回执，不要重复提交。"
	}
	run.SpawnAudit = spawnAudit(run)
	run.ResultState = "unconfirmed"
	switch run.Status {
	case RunSending:
		run.ResultState = "waiting"
	case RunFailed:
		run.ResultState = "failed"
	case RunSucceeded:
		switch {
		case run.Output == nil:
			run.ResultState = "output_unavailable"
		case run.Output.Text == "" && !run.Output.Truncated:
			run.ResultState = "no_output"
		default:
			run.ResultState = "output"
		}
	}
	if (run.Status == RunUncertain || run.Status == RunUnresponsive) && run.MayHaveExecuted && !run.CreatedAt.IsZero() {
		until := run.CreatedAt.Add(receiptRefreshWindow)
		run.ReceiptRefreshUntil = &until
	}
	return run
}

func receiptCompletion(receipt dstruntime.CommandReceipt) ExecutionCompletion {
	observedAt := receipt.CompletedAt.UTC()
	completion := ExecutionCompletion{
		Status: RunSucceeded, TransportOutcome: "sent", ExecutionOutcome: "confirmed",
		Message: "命令已由 DST 执行并返回确认", ObservedAt: &observedAt, Output: commandOutput(receipt.Details),
	}
	if !receipt.OK {
		completion.Status, completion.ExecutionOutcome = RunFailed, "rejected"
		completion.Message, completion.ErrorCode = "DST 未能执行命令", receipt.Code
		completion.ErrorMessage = strings.TrimSpace(receipt.Message)
		if completion.ErrorMessage == "" {
			completion.ErrorMessage = receipt.Code
		}
	}
	return completion
}

// RefreshRun reads the original receipt once, only when explicitly requested.
// There is no command dispatch, recovery probe, background poll, or replay.
// Concurrent reads of one run share the bounded Runtime read.
func (s *Service) RefreshRun(ctx context.Context, roomID, runID string) (Run, error) {
	run, err := s.store.Get(runID)
	if err != nil {
		return Run{}, err
	}
	if run.RoomID != roomID {
		return Run{}, ErrRunNotFound
	}
	if run.ReceiptRefreshUntil == nil {
		return run, nil
	}
	value, err, _ := s.receiptReads.Do(run.ID, func() (interface{}, error) {
		current, err := s.store.Get(run.ID)
		if err != nil {
			return Run{}, err
		}
		if current.ReceiptRefreshUntil == nil {
			return current, nil
		}
		now := s.store.now().UTC()
		check := func(status string) (Run, error) {
			current.ReceiptCheck = &ReceiptCheck{Status: status, CheckedAt: now}
			return current, nil
		}
		if !now.Before(*current.ReceiptRefreshUntil) {
			return check("expired")
		}
		reader, ok := s.commander.(receiptReader)
		if !ok {
			return check("unsupported")
		}
		readCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		request := dstruntime.CommandRequest{RequestID: current.ID, Action: "console.execute"}
		receipt, err := reader.ReadCommandReceipt(readCtx, current.RoomID, current.WorldID, request, current.CreatedAt)
		if readCtx.Err() != nil {
			return check("unavailable")
		}
		if err != nil {
			if errors.Is(err, dstruntime.ErrRuntimeResultAbsent) || errors.Is(err, os.ErrNotExist) {
				return check("not_found")
			}
			return check("unavailable")
		}
		// Adapters validate session/shard/module/time. Keep request correlation here
		// as well so a faulty reader cannot attach another command's output.
		if receipt.RequestID != current.ID || receipt.Action != request.Action || receipt.CompletedAt.IsZero() ||
			receipt.CompletedAt.Before(current.CreatedAt.Truncate(time.Second)) || receipt.CompletedAt.After(s.store.now().UTC().Add(30*time.Second)) {
			return check("not_found")
		}
		confirmed, err := s.store.confirmLateExecution(current.ID, receiptCompletion(receipt))
		if err != nil {
			return Run{}, err
		}
		confirmed.ReceiptCheck = &ReceiptCheck{Status: "found", CheckedAt: s.store.now().UTC()}
		return confirmed, nil
	})
	if err != nil {
		return Run{}, err
	}
	return value.(Run), nil
}
