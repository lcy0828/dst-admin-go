package softwareupdate

import (
	"context"
	"time"
)

// Only a program switch still in progress needs short boot-time observation.
// Ordinary service starts create no permanent poller or network requests.
func (s *Service) observeBootOperation(ctx context.Context) {
	value, err := s.Snapshot()
	if err != nil || value.Operation == nil || value.Operation.Phase != "restarting" {
		return
	}
	timeout, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-timeout.Done():
			return
		case <-ticker.C:
		}
		value, err = s.Snapshot()
		if err == nil && (value.Operation == nil || value.Operation.Phase != "restarting") {
			return
		}
	}
}
