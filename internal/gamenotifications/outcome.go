package gamenotifications

// Old successful records prove submission only. Do not infer execution from
// their legacy status, message text, timestamps, or skipped world records.
func deliveryOutcome(value Delivery) Outcome {
	if value.Outcome != "" {
		return value.Outcome
	}
	switch value.Status {
	case DeliverySucceeded:
		return OutcomeSubmitted
	case DeliveryFailed:
		if value.ErrorCode == "NOTIFICATION_SEND_FAILED" || value.ErrorCode == "SERVICE_RESTARTED" {
			return OutcomeUnconfirmed
		}
		return OutcomeFailed
	case DeliverySkipped:
		return OutcomeSkipped
	case DeliveryCanceled:
		return OutcomeCanceled
	default:
		return OutcomeWaiting
	}
}

func describeNotification(value Notification) Notification {
	value.SubmittedCount, value.ConfirmedCount, value.UnconfirmedCount = 0, 0, 0
	var broadcast Outcome
	failed, waiting := false, false
	for index := range value.Deliveries {
		delivery := &value.Deliveries[index]
		delivery.Outcome = deliveryOutcome(*delivery)
		switch delivery.Outcome {
		case OutcomeConfirmed:
			value.ConfirmedCount++
			broadcast = OutcomeConfirmed
		case OutcomeSubmitted:
			value.SubmittedCount++
			if broadcast != OutcomeConfirmed {
				broadcast = OutcomeSubmitted
			}
		case OutcomeUnconfirmed:
			value.UnconfirmedCount++
			if broadcast == "" {
				broadcast = OutcomeUnconfirmed
			}
		case OutcomeFailed:
			failed = true
		case OutcomeWaiting:
			waiting = true
		}
	}
	// A room broadcast needs just one execution. Another world's availability
	// or a later cancellation cannot invalidate evidence of that execution.
	switch {
	case broadcast != "":
		value.Outcome = broadcast
	case value.Status == StatusQueued || value.Status == StatusSending || waiting:
		value.Outcome = OutcomeWaiting
	case value.Status == StatusCanceled:
		value.Outcome = OutcomeCanceled
	case failed || value.Status == StatusFailed:
		value.Outcome = OutcomeFailed
	case value.Status == StatusSucceeded || value.Status == StatusPartial:
		value.Outcome = OutcomeSubmitted
	default:
		value.Outcome = OutcomeSkipped
	}
	return value
}
