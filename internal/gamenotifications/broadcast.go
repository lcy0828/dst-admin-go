package gamenotifications

import (
	"context"
	"errors"
	"time"

	"dont/internal/dstruntime"
	"dont/shared"
)

const broadcastTimeout = 8 * time.Second

func unconfirmedDelivery(delivery Delivery, cause error) Delivery {
	delivery.Status, delivery.Outcome = DeliveryFailed, OutcomeUnconfirmed
	delivery.ErrorCode = "NOTIFICATION_OUTCOME_UNKNOWN"
	delivery.Message = "未能确认游戏是否执行公告；不会自动重发"
	delivery.ErrorMessage = delivery.Message
	if cause != nil {
		delivery.ErrorMessage += ": " + cause.Error()
	}
	return delivery
}

func failedDelivery(delivery Delivery, cause error) Delivery {
	delivery.Status, delivery.Outcome = DeliveryFailed, OutcomeFailed
	delivery.ErrorCode, delivery.Message = "NOTIFICATION_SEND_FAILED", "公告未能发送"
	if cause != nil {
		delivery.ErrorMessage = cause.Error()
	}
	return delivery
}

func (s *Service) sendBroadcast(ctx context.Context, notification Notification, delivery Delivery) Delivery {
	ctx, cancel := context.WithTimeout(ctx, broadcastTimeout)
	defer cancel()
	script := "c_announce(" + quoteLua(notification.Message) + ")"
	if s.commander == nil {
		// Compatibility adapter: acknowledgement of a console submission alone
		// never becomes execution evidence, even if its transport says confirmed.
		result, err := s.runtime.SendID(ctx, notification.RoomID, delivery.WorldID, shared.RuntimeConsoleRequest{
			Mode: shared.ConsoleModeManaged, Command: script,
		})
		delivery.TargetID, delivery.AgentID, delivery.TopologyRevision = result.TargetID, result.AgentID, result.TopologyRevision
		if !result.ObservedAt.IsZero() {
			observed := result.ObservedAt.UTC()
			delivery.ObservedAt = &observed
		}
		if result.Outcome == shared.RuntimeOutcomeUnknown || err != nil &&
			(result.Outcome == shared.RuntimeOutcomeSent || result.Outcome == shared.RuntimeOutcomeConfirmed ||
				errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return unconfirmedDelivery(delivery, err)
		}
		if err != nil || result.Outcome == shared.RuntimeOutcomeFailed {
			return failedDelivery(delivery, err)
		}
		delivery.Status, delivery.Outcome = DeliverySucceeded, OutcomeSubmitted
		delivery.Message = "公告已提交，尚未确认游戏执行"
		return delivery
	}

	started := s.now().UTC()
	receipt, err := s.commander.ExecuteCommand(ctx, notification.RoomID, delivery.WorldID, dstruntime.CommandRequest{
		RequestID: notification.ID, Action: "console.execute", Arguments: map[string]interface{}{"script": script},
	})
	delivery.TargetID, delivery.AgentID, delivery.TopologyRevision = receipt.TargetID, receipt.AgentID, receipt.TopologyRevision
	if err != nil {
		if errors.Is(err, dstruntime.ErrRuntimeCommandUnconfirmed) || errors.Is(err, dstruntime.ErrRuntimeResultAbsent) {
			return unconfirmedDelivery(delivery, err)
		}
		return failedDelivery(delivery, err)
	}
	if receipt.RequestID != notification.ID || receipt.Action != "console.execute" || receipt.CompletedAt.IsZero() ||
		receipt.CompletedAt.Before(started.Truncate(time.Second)) || receipt.CompletedAt.After(s.now().UTC().Add(30*time.Second)) {
		return unconfirmedDelivery(delivery, dstruntime.ErrRuntimeResultInvalid)
	}
	observed := receipt.CompletedAt.UTC()
	delivery.ObservedAt = &observed
	if !receipt.OK {
		delivery = failedDelivery(delivery, errors.New(receipt.Message))
		delivery.ErrorCode = receipt.Code
		return delivery
	}
	delivery.Status, delivery.Outcome = DeliverySucceeded, OutcomeConfirmed
	delivery.Message = "游戏已执行房间广播"
	return delivery
}
