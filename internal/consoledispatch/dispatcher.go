package consoledispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidRequest  = errors.New("console dispatch request is invalid")
	ErrPaused          = errors.New("console dispatch is paused for this shard")
	ErrCapacityReached = errors.New("console dispatch capacity is reached for this shard")
	ErrInstanceChanged = errors.New("console target instance changed")
	ErrInputDirty      = errors.New("console input state is dirty")
	ErrExternalWriter  = errors.New("console has an unmanaged external writer")
	ErrMaintenanceBusy = errors.New("console maintenance is already active")
)

const DefaultPendingLimit = 32

type Class string

const (
	ClassForeground Class = "foreground"
	ClassBackground Class = "background"
)

type Request struct {
	Class       Class
	CoalesceKey string
	InstanceID  string
	Execute     func(context.Context) error
}

type Health struct {
	Status               string    `json:"status"`
	Accepting            bool      `json:"accepting"`
	Busy                 bool      `json:"busy"`
	Pending              int       `json:"pending"`
	PendingLimit         int       `json:"pendingLimit"`
	Class                Class     `json:"class,omitempty"`
	CoalesceKey          string    `json:"coalesceKey,omitempty"`
	StartedAt            time.Time `json:"startedAt,omitempty"`
	InstanceID           string    `json:"instanceId,omitempty"`
	Maintenance          bool      `json:"maintenance"`
	MaintenanceOwner     string    `json:"maintenanceOwner,omitempty"`
	MaintenanceStartedAt time.Time `json:"maintenanceStartedAt,omitempty"`
	InputDirty           bool      `json:"inputDirty"`
	ExternalWriter       bool      `json:"externalWriter"`
}

type result struct {
	done    chan struct{}
	err     error
	waiters int
}

type lane struct {
	token chan struct{}

	mu           sync.Mutex
	accepting    bool
	pending      int
	limit        int
	busy         bool
	class        Class
	coalesce     string
	startedAt    time.Time
	instance     string
	dirty        bool
	external     bool
	maintID      string
	maintOwner   string
	maintStarted time.Time
	shared       map[string]*result
}

type Dispatcher struct {
	mu    sync.Mutex
	lanes map[string]*lane
	now   func() time.Time
	limit int
}

func New() *Dispatcher {
	return NewWithPendingLimit(DefaultPendingLimit)
}

func NewWithPendingLimit(limit int) *Dispatcher {
	if limit < 1 {
		limit = DefaultPendingLimit
	}
	return &Dispatcher{lanes: make(map[string]*lane), now: time.Now, limit: limit}
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
	request.InstanceID = strings.TrimSpace(request.InstanceID)
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
	if lane.dirty {
		lane.mu.Unlock()
		return ErrInputDirty
	}
	if lane.external {
		lane.mu.Unlock()
		return ErrExternalWriter
	}
	if lane.maintID != "" {
		lane.mu.Unlock()
		return ErrMaintenanceBusy
	}
	lane.accepting = true
	lane.mu.Unlock()
	return nil
}

// BindInstance advances an idle lane to a specific runtime instance. Commands
// queued for an older instance are rejected instead of crossing a restart.
func (d *Dispatcher) BindInstance(shardKey, instanceID string) error {
	shardKey, instanceID = strings.TrimSpace(shardKey), strings.TrimSpace(instanceID)
	if shardKey == "" || instanceID == "" {
		return ErrInvalidRequest
	}
	lane := d.lane(shardKey)
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.instance == instanceID {
		return nil
	}
	if lane.busy || lane.pending > 0 {
		return ErrInstanceChanged
	}
	lane.instance = instanceID
	lane.dirty = false
	lane.external = false
	return nil
}

// BeginMaintenance closes the lane to normal writers and waits for the active
// writer to finish. The returned lease must be released before dispatch resumes.
func (d *Dispatcher) BeginMaintenance(ctx context.Context, shardKey, owner, instanceID string) (*MaintenanceLease, error) {
	shardKey, owner, instanceID = strings.TrimSpace(shardKey), strings.TrimSpace(owner), strings.TrimSpace(instanceID)
	if ctx == nil || shardKey == "" || owner == "" {
		return nil, ErrInvalidRequest
	}
	lane := d.lane(shardKey)
	lane.mu.Lock()
	if lane.maintID != "" {
		lane.mu.Unlock()
		return nil, ErrMaintenanceBusy
	}
	if instanceID != "" && lane.instance != "" && lane.instance != instanceID {
		lane.mu.Unlock()
		return nil, ErrInstanceChanged
	}
	lane.accepting = false
	lane.maintID = maintenanceID(d.now())
	lane.maintOwner = owner
	lane.maintStarted = d.now().UTC()
	leaseID := lane.maintID
	lane.mu.Unlock()
	select {
	case <-ctx.Done():
		d.cancelMaintenance(shardKey, leaseID)
		return nil, ctx.Err()
	case <-lane.token:
		lane.token <- struct{}{}
		return &MaintenanceLease{dispatcher: d, shardKey: shardKey, id: leaseID}, nil
	}
}

type MaintenanceLease struct {
	dispatcher *Dispatcher
	shardKey   string
	id         string
	once       sync.Once
	err        error
}

// Release reopens the lane only when its input and external-writer checks are clean.
func (l *MaintenanceLease) Release() error {
	if l == nil || l.dispatcher == nil {
		return ErrInvalidRequest
	}
	l.once.Do(func() { l.err = l.dispatcher.releaseMaintenance(l.shardKey, l.id) })
	return l.err
}

func (d *Dispatcher) MarkInputDirty(shardKey string)     { d.setHazard(shardKey, true, false) }
func (d *Dispatcher) MarkExternalWriter(shardKey string) { d.setHazard(shardKey, false, true) }

func (d *Dispatcher) setHazard(shardKey string, dirty, external bool) {
	shardKey = strings.TrimSpace(shardKey)
	if shardKey == "" {
		return
	}
	lane := d.lane(shardKey)
	lane.mu.Lock()
	lane.dirty = lane.dirty || dirty
	lane.external = lane.external || external
	lane.accepting = false
	lane.mu.Unlock()
}

func (d *Dispatcher) Health(shardKey string) Health {
	lane := d.lane(strings.TrimSpace(shardKey))
	lane.mu.Lock()
	defer lane.mu.Unlock()
	status := "ready"
	switch {
	case lane.dirty:
		status = "input_dirty"
	case lane.external:
		status = "external_writer"
	case lane.maintID != "":
		status = "maintenance"
	case !lane.accepting:
		status = "paused"
	case lane.pending >= lane.limit:
		status = "capacity_reached"
	}
	return Health{
		Status: status, Accepting: lane.accepting, Busy: lane.busy, Pending: lane.pending, PendingLimit: lane.limit,
		Class: lane.class, CoalesceKey: lane.coalesce, StartedAt: lane.startedAt, InstanceID: lane.instance,
		Maintenance: lane.maintID != "", MaintenanceOwner: lane.maintOwner, MaintenanceStartedAt: lane.maintStarted,
		InputDirty: lane.dirty, ExternalWriter: lane.external,
	}
}

func (d *Dispatcher) lane(key string) *lane {
	d.mu.Lock()
	defer d.mu.Unlock()
	if current := d.lanes[key]; current != nil {
		return current
	}
	created := &lane{token: make(chan struct{}, 1), accepting: true, limit: d.limit, shared: make(map[string]*result)}
	created.token <- struct{}{}
	d.lanes[key] = created
	return created
}

func (l *lane) reserve(request Request) (*result, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.accepting {
		if l.dirty {
			return nil, false, ErrInputDirty
		}
		if l.external {
			return nil, false, ErrExternalWriter
		}
		return nil, false, ErrPaused
	}
	if request.CoalesceKey != "" {
		if existing := l.shared[request.CoalesceKey]; existing != nil {
			existing.waiters++
			return existing, false, nil
		}
	}
	if l.pending >= l.limit {
		return nil, false, ErrCapacityReached
	}
	if request.InstanceID != "" {
		if l.instance == "" {
			l.instance = request.InstanceID
		} else if l.instance != request.InstanceID {
			return nil, false, ErrInstanceChanged
		}
	}
	created := &result{done: make(chan struct{}), waiters: 1}
	if request.CoalesceKey != "" {
		l.shared[request.CoalesceKey] = created
	}
	l.pending++
	return created, true, nil
}

func (d *Dispatcher) cancelMaintenance(shardKey, leaseID string) {
	lane := d.lane(shardKey)
	lane.mu.Lock()
	if lane.maintID == leaseID {
		lane.maintID, lane.maintOwner, lane.maintStarted = "", "", time.Time{}
		lane.accepting = !lane.dirty && !lane.external
	}
	lane.mu.Unlock()
}

func (d *Dispatcher) releaseMaintenance(shardKey, leaseID string) error {
	lane := d.lane(shardKey)
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.maintID == "" || lane.maintID != leaseID {
		return ErrInvalidRequest
	}
	lane.maintID, lane.maintOwner, lane.maintStarted = "", "", time.Time{}
	if lane.dirty {
		return ErrInputDirty
	}
	if lane.external {
		return ErrExternalWriter
	}
	lane.accepting = true
	return nil
}

func maintenanceID(now time.Time) string {
	return now.UTC().Format("20060102T150405.000000000")
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
