package players

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/shards"
)

const passivePlayerInterval = 5 * time.Second

var errPassivePlayersBusy = errors.New("player snapshot synchronization is busy")

type passivePlayerRead struct {
	done    chan struct{}
	expires time.Time
	err     error
}

type passivePlayerReads struct {
	mu    sync.Mutex
	rooms map[string]*passivePlayerRead
	slots chan struct{}
}

type passivePlayerProbe interface {
	PassiveSnapshot(context.Context, string, string) (SnapshotResult, error)
}

// PassiveSnapshot consumes existing artifacts only. In particular, a read API
// must never repair the Runtime, send Lua, or fall back to a console probe.
func (p *TelemetryProbe) PassiveSnapshot(ctx context.Context, roomID, worldID string) (SnapshotResult, error) {
	snapshot, err := p.runtime.ReadPlayers(ctx, roomID, worldID)
	if err != nil {
		return SnapshotResult{}, err
	}
	return inventoryRuntimeSnapshot(snapshot), nil
}

// SyncPassivePlayers lets UI and MCP reads observe fresh Runtime output even
// when the optional scheduled refresh task is disabled. Work is demand-driven,
// shared per room for five seconds, and limited to two rooms at a time.
func (s *Service) SyncPassivePlayers(ctx context.Context, roomID string) error {
	probe, supported := s.probe.(passivePlayerProbe)
	if !supported {
		return nil
	}
	room, err := s.managedRoom(roomID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reads := &s.passiveReads
	reads.mu.Lock()
	if previous := reads.rooms[room.ID]; previous != nil {
		select {
		case <-previous.done:
			if s.now().Before(previous.expires) {
				reads.mu.Unlock()
				return previous.err
			}
		default:
			reads.mu.Unlock()
			select {
			case <-previous.done:
				return previous.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if reads.rooms == nil {
		reads.rooms = make(map[string]*passivePlayerRead)
		reads.slots = make(chan struct{}, 2)
	}
	for id, entry := range reads.rooms {
		if !entry.expires.IsZero() && !s.now().Before(entry.expires) {
			delete(reads.rooms, id)
		}
	}
	if len(reads.rooms) >= 128 {
		reads.mu.Unlock()
		return errPassivePlayersBusy
	}
	select {
	case reads.slots <- struct{}{}:
	default:
		reads.mu.Unlock()
		return errPassivePlayersBusy
	}
	entry := &passivePlayerRead{done: make(chan struct{})}
	reads.rooms[room.ID] = entry
	reads.mu.Unlock()
	defer func() {
		reads.mu.Lock()
		entry.err, entry.expires = err, s.now().Add(passivePlayerInterval)
		close(entry.done)
		<-reads.slots
		reads.mu.Unlock()
	}()

	lock := s.roomLock(room.ID)
	if !lock.TryLock() {
		err = errPassivePlayersBusy
		return err
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	worlds, err := s.rooms.Worlds(room.ID)
	if err != nil {
		return err
	}
	if len(worlds) > 64 {
		err = ErrInvalidFilter
		return err
	}
	snapshots := make([]worldSnapshot, 0, len(worlds))
	var stale []string
	for index, world := range worlds {
		if ctx.Err() != nil {
			for _, pending := range worlds[index:] {
				stale = append(stale, pending.ID)
			}
			err = errors.Join(err, ctx.Err())
			break
		}
		status, readErr := s.worldStatus(ctx, room, world)
		if readErr == nil && (status.State == shards.RuntimeStopped || status.State == shards.RuntimeFailed) {
			snapshots = append(snapshots, worldSnapshot{WorldID: world.ID, WorldName: world.Name, Stopped: true, ObservedAt: s.now().UTC()})
			continue
		}
		var result SnapshotResult
		if readErr == nil && status.State == shards.RuntimeRunning {
			result, readErr = probe.PassiveSnapshot(ctx, room.ID, world.ID)
			if ctx.Err() != nil {
				readErr = ctx.Err()
			}
			age := s.now().Sub(result.ObservedAt)
			if readErr == nil && (result.Degraded || !result.LocalityKnown || !observationsHaveLocality(result.Observations) || result.SessionID == "" || age < -2*time.Second || age > 20*time.Second) {
				readErr = dstruntime.ErrSnapshotStale
			}
			if readErr == nil {
				readErr = validateObservations(result.Observations)
			}
		} else if readErr == nil {
			readErr = errors.New("world player presence is not yet confirmed")
		}
		if readErr != nil {
			stale = append(stale, world.ID)
			err = errors.Join(err, fmt.Errorf("%s: %w", world.Name, readErr))
			continue
		}
		snapshots = append(snapshots, worldSnapshot{WorldID: world.ID, WorldName: world.Name,
			SessionID: result.SessionID, ObservedAt: result.ObservedAt, Source: result.Source, Observations: result.Observations})
	}
	if len(snapshots) > 0 {
		err = errors.Join(err, s.store.ReplaceRoomSnapshots(room.ID, snapshots))
	}
	err = errors.Join(err, s.store.ObserveRuntimePresence(room.ID, nil, stale, s.now().UTC()))
	return err
}
