package notifications

import (
	"context"
	"strconv"
	"time"

	"dont/internal/gameupdate"
)

// A reader leases demand for two minutes. No subscriber means no timer or game
// version probes. All groups share one check, at most every thirty minutes.
func (s *Service) watchGame(room string) {
	s.mu.Lock()
	if s.gameWatches == nil {
		s.gameWatches = map[string]time.Time{}
	}
	if len(s.gameWatches) < 128 || !s.gameWatches[room].IsZero() {
		s.gameWatches[room] = time.Now().Add(2 * time.Minute)
	}
	s.mu.Unlock()
	select {
	case s.gameWake <- struct{}{}:
	default:
	}
}
func (s *Service) watchedGames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rooms := []string{}
	for room, until := range s.gameWatches {
		if time.Now().Before(until) {
			rooms = append(rooms, room)
		} else {
			delete(s.gameWatches, room)
		}
	}
	return rooms
}
func (s *Service) RunGameChecks(ctx context.Context, check func(context.Context, []string)) {
	var next time.Time
	for ctx.Err() == nil {
		rooms := s.watchedGames()
		if len(rooms) > 0 && !time.Now().Before(next) {
			read, cancel := context.WithTimeout(ctx, 30*time.Second)
			check(read, rooms)
			cancel()
			next = time.Now().Add(30 * time.Minute)
		}
		var tick <-chan time.Time
		var timer *time.Timer
		if len(rooms) > 0 {
			timer = time.NewTimer(min(time.Until(next), 2*time.Minute))
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.gameWake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// Compare official game numbers only with actual public-branch observations.
// Steam build IDs and game versions are different number spaces.
func (s *Service) ObserveGameVersions(release gameupdate.OfficialRelease, versions []gameupdate.InstalledVersion) {
	latest, err := strconv.ParseUint(release.Version, 10, 64)
	if err != nil || release.Stale || release.Version == "" {
		return
	}
	for _, value := range versions {
		current, err := strconv.ParseUint(value.GameVersion, 10, 64)
		if err != nil || !value.Installed || !value.Online || value.Error != "" || value.Branch != "public" || current >= latest {
			continue
		}
		for _, room := range value.RoomIDs {
			s.fact(Event{ID: "official-game:" + room + ":" + release.Version, Kind: "game.available", Category: "updates", RoomID: room, Version: release.Version, Status: "available", Severity: "info", Title: "发现游戏新版", Message: "游戏版本 " + value.GameVersion + " → " + release.Version + "；更新时需要暂时停止相关世界。", OccurredAt: release.CheckedAt, Facts: &Facts{Action: "game.update"}})
		}
	}
}
