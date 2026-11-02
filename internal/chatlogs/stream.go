package chatlogs

import (
	"context"
	"strings"
	"sync"
	"time"
)

const streamMaxSubscribers = 32
const streamMaxRooms = 8
const streamMaxPerRoom = 8

type StreamStatus struct {
	State        string     `json:"state"`
	Partial      bool       `json:"partial"`
	LastSyncedAt *time.Time `json:"lastSyncedAt,omitempty"`
}
type chatFeed struct {
	cancel    context.CancelFunc
	done      <-chan struct{}
	listeners map[chan struct{}]struct{}
	status    StreamStatus
}
type Subscription struct {
	Initial StreamWindow
	Wake    <-chan struct{}
	Done    <-chan struct{}
	close   func()
}

func (s *Subscription) Close() {
	if s.close != nil {
		s.close()
	}
}

func (s *Service) validateStream(roomID string, kind Kind) error {
	if s.store == nil || s.generations == nil {
		return ErrStreamUnavailable
	}
	if kind != "" && kind != KindSay && kind != KindWhisper && kind != KindAnnouncement {
		return ErrInvalidFilter
	}
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return err
	}
	if !room.Managed {
		return ErrRoomNotManaged
	}
	return nil
}

func (s *Service) Subscribe(ctx context.Context, roomID string, kind Kind, cursor string) (*Subscription, error) {
	roomID = strings.TrimSpace(roomID)
	if err := s.validateStream(roomID, kind); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	initial, err := s.store.StreamWindow(roomID, kind, cursor)
	if err != nil {
		return nil, err
	}
	s.feedsMu.Lock()
	defer s.feedsMu.Unlock()
	feed := s.feeds[roomID]
	if s.feedsClosed || s.feedListeners >= streamMaxSubscribers || feed != nil && len(feed.listeners) >= streamMaxPerRoom || feed == nil && len(s.feeds) >= streamMaxRooms {
		return nil, ErrStreamBusy
	}
	fresh := feed == nil
	var feedCtx context.Context
	if fresh {
		var cancel context.CancelFunc
		feedCtx, cancel = context.WithCancel(context.Background())
		feed = &chatFeed{cancel: cancel, done: feedCtx.Done(), listeners: make(map[chan struct{}]struct{}), status: StreamStatus{State: "syncing", Partial: true}}
		s.feeds[roomID] = feed
	}
	wake := make(chan struct{}, 1)
	feed.listeners[wake] = struct{}{}
	s.feedListeners++
	var once sync.Once
	sub := &Subscription{Initial: initial, Wake: wake, Done: feed.done}
	sub.close = func() {
		once.Do(func() {
			s.feedsMu.Lock()
			defer s.feedsMu.Unlock()
			delete(feed.listeners, wake)
			s.feedListeners--
			if len(feed.listeners) == 0 {
				feed.cancel()
				if s.feeds[roomID] == feed {
					delete(s.feeds, roomID)
				}
			}
		})
	}
	if fresh {
		s.feedsWG.Add(1)
		go s.collectChatFeed(feedCtx, roomID, feed)
	}
	// A commit between capturing the tail and registration must still be read.
	wake <- struct{}{}
	return sub, nil
}
func (s *Service) StreamWindow(ctx context.Context, roomID string, kind Kind, cursor string) (StreamWindow, error) {
	if err := ctx.Err(); err != nil {
		return StreamWindow{}, err
	}
	if err := s.validateStream(roomID, kind); err != nil {
		return StreamWindow{}, err
	}
	return s.store.StreamWindow(roomID, kind, cursor)
}
func (s *Service) StreamStatus(roomID string) StreamStatus {
	s.feedsMu.Lock()
	defer s.feedsMu.Unlock()
	if feed := s.feeds[roomID]; feed != nil {
		return feed.status
	}
	return StreamStatus{State: "unavailable", Partial: true}
}
func (s *Service) notifyChatFeed(roomID string) {
	s.feedsMu.Lock()
	defer s.feedsMu.Unlock()
	if feed := s.feeds[roomID]; feed != nil {
		for wake := range feed.listeners {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
}
func (s *Service) collectChatFeed(ctx context.Context, roomID string, feed *chatFeed) {
	defer s.feedsWG.Done()
	pause := 2 * time.Second
	for {
		select {
		case s.feedBudget <- struct{}{}:
		case <-ctx.Done():
			return
		}
		var result SyncResult
		var err error
		if s.shouldSync(roomID, time.Now()) {
			result, _, err = s.syncRoom(ctx, roomID, false)
		}
		<-s.feedBudget
		if ctx.Err() != nil {
			return
		}
		state, readErr := s.store.SyncState(roomID)
		status := StreamStatus{State: state.State, LastSyncedAt: state.LastSyncedAt, Partial: state.State != "ready"}
		if err != nil || readErr != nil {
			status.State = "unavailable"
			status.Partial = true
		}
		if status.State == "" || status.State == "pending" {
			status.State = "syncing"
		}
		if len(result.Problems) > 0 && result.AvailableWorlds == 0 || err != nil || readErr != nil {
			pause = min(pause*2, 30*time.Second)
		} else {
			pause = 2 * time.Second
		}
		s.feedsMu.Lock()
		feed.status = status
		for wake := range feed.listeners {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		s.feedsMu.Unlock()
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Shutdown cancels only chat subscribers' demand-driven collectors.
func (s *Service) Close() {
	s.feedsMu.Lock()
	s.feedsClosed = true
	for _, feed := range s.feeds {
		feed.cancel()
	}
	s.feedsMu.Unlock()
	s.feedsWG.Wait()
}
