package consoledispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidRequest = errors.New("console dispatch request is invalid")
	ErrPaused         = errors.New("console dispatch is paused for this shard")
)

type Class string

const (
	ClassForeground Class = "foreground"
	ClassBackground Class = "background"
)

type Request struct {
	Class       Class
	CoalesceKey string
	Execute     func(context.Context) error
}

type Health struct {
	Accepting   bool      `json:"accepting"`
	Busy        bool      `json:"busy"`
	Pending     int       `json:"pending"`
	Class       Class     `json:"class,omitempty"`
	CoalesceKey string    `json:"coalesceKey,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
}

type result struct {
	done    chan struct{}
	err     error
	waiters int
}

type lane struct {
	token chan struct{}

	mu        sync.Mutex
	accepting bool
	pending   int
	busy      bool
	class     Class
	coalesce  string
	startedAt time.Time
	shared    map[string]*result
}

type Dispatcher struct {
	mu    sync.Mutex
	lanes map[string]*lane
	now   func() time.Time
}

func New() *Dispatcher {
	return &Dispatcher{lanes: make(map[string]*lane), now: time.Now}
}

func (d *Dispatcher) Dispatch(ctx context.Context, shardKey string, request Request) error {
	shardKey = strings.TrimSpace(shardKey)
	if ctx == nil || shardKey == "" || request.Execute == nil {
		return ErrInvalidRequest
	}
	if request.Class == "" {
		request.Class = ClassForeground
	}
	if request.Class != ClassForeground && request.Class != ClassBackground {
		return ErrInvalidRequest
	}
	request.CoalesceKey = strings.TrimSpace(request.CoalesceKey)
	if request.Class != ClassBackground && request.CoalesceKey != "" {
		return ErrInvalidRequest
	}

	lane := d.lane(shardKey)
	sharedResult, owner, err := lane.reserve(request)
	if err != nil {
		return err
	}
	if !owner {
		return waitResult(ctx, sharedResult)
	}

	err = d.execute(ctx, lane, request)
	lane.complete(request.CoalesceKey, sharedResult, err)
	return err
}

func (d *Dispatcher) execute(ctx context.Context, lane *lane, request Request) error {
	select {
	case <-ctx.Done():
		lane.cancelPending()
		return ctx.Err()
	case <-lane.token:
	}
	defer func() { lane.token <- struct{}{} }()

	lane.mu.Lock()
	lane.pending--
	if !lane.accepting {
		lane.mu.Unlock()
		return ErrPaused
	}
	lane.busy = true
	lane.class = request.Class
	lane.coalesce = request.CoalesceKey
	lane.startedAt = d.now().UTC()
	lane.mu.Unlock()

	err := request.Execute(ctx)
	lane.mu.Lock()
	lane.busy = false
	lane.class = ""
	lane.coalesce = ""
	lane.startedAt = time.Time{}
	lane.mu.Unlock()
	return err
}

// Pause rejects new work and waits until any command already writing to the
// shard console has completed. A later Resume reopens the lane.
func (d *Dispatcher) Pause(ctx context.Context, shardKey string) error {
	shardKey = strings.TrimSpace(shardKey)
	if ctx == nil || shardKey == "" {
		return ErrInvalidRequest
	}
	lane := d.lane(shardKey)
	lane.mu.Lock()
	lane.accepting = false
	lane.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-lane.token:
		lane.token <- struct{}{}
		return nil
	}
}

func (d *Dispatcher) Resume(shardKey string) error {
	shardKey = strings.TrimSpace(shardKey)
	if shardKey == "" {
		return ErrInvalidRequest
	}
	lane := d.lane(shardKey)
	lane.mu.Lock()
	lane.accepting = true
	lane.mu.Unlock()
	return nil
}

func (d *Dispatcher) Health(shardKey string) Health {
	lane := d.lane(strings.TrimSpace(shardKey))
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return Health{
		Accepting: lane.accepting, Busy: lane.busy, Pending: lane.pending,
		Class: lane.class, CoalesceKey: lane.coalesce, StartedAt: lane.startedAt,
	}
}

func (d *Dispatcher) lane(key string) *lane {
	d.mu.Lock()
	defer d.mu.Unlock()
	if current := d.lanes[key]; current != nil {
		return current
	}
	created := &lane{token: make(chan struct{}, 1), accepting: true, shared: make(map[string]*result)}
	created.token <- struct{}{}
	d.lanes[key] = created
	return created
}

func (l *lane) reserve(request Request) (*result, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.accepting {
		return nil, false, ErrPaused
	}
	if request.CoalesceKey != "" {
		if existing := l.shared[request.CoalesceKey]; existing != nil {
			existing.waiters++
			return existing, false, nil
		}
	}
	created := &result{done: make(chan struct{}), waiters: 1}
	if request.CoalesceKey != "" {
		l.shared[request.CoalesceKey] = created
	}
	l.pending++
	return created, true, nil
}

func (l *lane) cancelPending() {
	l.mu.Lock()
	l.pending--
	l.mu.Unlock()
}

func (l *lane) complete(coalesceKey string, value *result, err error) {
	l.mu.Lock()
	if coalesceKey != "" {
		delete(l.shared, coalesceKey)
	}
	value.err = err
	close(value.done)
	l.mu.Unlock()
}

func waitResult(ctx context.Context, value *result) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-value.done:
		return value.err
	}
}
