package modupdates

import (
	"context"
	"errors"
	"slices"

	"dont/internal/maintenance"
	"dont/internal/players"
	"dont/internal/roomops"
)

type affectedRoomCatalog interface {
	AffectedRoomIDs(context.Context, string) ([]string, error)
}

type maintenanceScopeKey struct{}
type lockedMaintenanceScope struct {
	roomID string
	ids    []string
}

func (s *Service) ConfigureOnlineCounter(counter maintenance.OnlineCounter) { s.online = counter }

func (s *Service) affectedRooms(ctx context.Context, roomID string) ([]string, error) {
	if catalog, ok := s.catalog.(affectedRoomCatalog); ok {
		return catalog.AffectedRoomIDs(ctx, roomID)
	}
	return []string{roomID}, nil
}

func (s *Service) lockAffectedRooms(ctx context.Context, roomID string) (context.Context, func(), []string, error) {
	ids, err := s.affectedRooms(ctx, roomID)
	if err != nil {
		return ctx, nil, nil, err
	}
	ids = normalizedIDs(ids)
	if len(ids) == 0 {
		return ctx, nil, nil, maintenance.ErrPresenceUnavailable
	}
	if held, ok := ctx.Value(maintenanceScopeKey{}).(lockedMaintenanceScope); ok && held.roomID == roomID {
		if !slices.Equal(ids, held.ids) {
			return ctx, nil, nil, errors.New("房间运行位置已变化，等待下次检查")
		}
		return ctx, func() {}, ids, nil
	}
	ctx, release, err := roomops.AcquireMany(ctx, ids)
	if err != nil {
		return ctx, nil, nil, err
	}
	fresh, err := s.affectedRooms(ctx, roomID)
	if err == nil && !slices.Equal(ids, normalizedIDs(fresh)) {
		err = errors.New("房间运行位置已变化，等待下次检查")
	}
	if err != nil {
		release()
		return ctx, nil, nil, err
	}
	return context.WithValue(ctx, maintenanceScopeKey{}, lockedMaintenanceScope{roomID: roomID, ids: ids}), release, ids, nil
}

func (s *Service) refreshAffectedPresence(ctx context.Context, ids []string) (players.PresenceSnapshot, error) {
	result := players.PresenceSnapshot{Fresh: true}
	for _, id := range ids {
		var current players.PresenceSnapshot
		var err error
		if s.online != nil {
			current.Online, err = s.online.OnlinePlayers(ctx, id)
			current.Fresh = err == nil && current.Online >= 0
		} else {
			current, err = s.presence.RefreshPresence(ctx, id)
		}
		if err != nil {
			return result, err
		}
		result.Online += current.Online
		result.StaleOnline += current.StaleOnline
		result.Fresh = result.Fresh && current.Fresh
	}
	return result, nil
}

func (s *Service) requireEmpty(ctx context.Context, ids []string) error {
	presence, err := s.refreshAffectedPresence(ctx, ids)
	if err != nil {
		return errors.Join(maintenance.ErrPresenceUnavailable, err)
	}
	if !presence.Fresh {
		return maintenance.ErrPresenceUnavailable
	}
	if presence.Online > 0 {
		return maintenance.ErrPlayersOnline
	}
	return nil
}
