package runtimeobservation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dont/internal/agents"
	"dont/shared"
)

const (
	defaultRefreshInterval = 30 * time.Second
	defaultFreshnessWindow = 90 * time.Second
	defaultReleaseGrace    = 60 * time.Second
	defaultCollectTimeout  = 25 * time.Second
	defaultCheckpointEvery = 5 * time.Minute
)

var (
	ErrRuntimeTargetNotFound  = errors.New("runtime observation target not found")
	ErrObservationUnavailable = errors.New("runtime observation is temporarily unavailable")
)

type UnavailableError struct {
	TargetID       string
	InstallationID string
	State          State
	Message        string
}

func (e *UnavailableError) Error() string {
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return ErrObservationUnavailable.Error()
}

func (*UnavailableError) Unwrap() error { return ErrObservationUnavailable }

type Source interface {
	CachedRuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error)
	CollectRuntimeTargetInventory(context.Context, string, string) (agents.RuntimeTargetInventory, error)
	CheckpointRuntimeTargetInventory(string, shared.RuntimeInventoryReport) error
}

type Options struct {
	RefreshInterval   time.Duration
	FreshnessWindow   time.Duration
	ReleaseGrace      time.Duration
	CollectTimeout    time.Duration
	CheckpointEvery   time.Duration
	RemoteConcurrency int
	Now               func() time.Time
	Broker            *Broker
}

type cacheEntry struct {
	inventory      agents.RuntimeTargetInventory
	state          State
	err            string
	refreshStarted *time.Time
	lastCheckpoint time.Time
	checkpointHash string
}

type flight struct {
	done    chan struct{}
	result  RefreshResult
	waiters int
}

type lease struct {
	refs   int
	cancel context.CancelFunc
	timer  *time.Timer
}

type Coordinator struct {
	source          Source
	broker          *Broker
	now             func() time.Time
	refreshInterval time.Duration
	freshnessWindow time.Duration
	releaseGrace    time.Duration
	collectTimeout  time.Duration
	checkpointEvery time.Duration
	remoteSlots     chan struct{}
	localSlots      chan struct{}

	mu       sync.Mutex
	cache    map[string]cacheEntry
	flights  map[string]*flight
	leases   map[string]*lease
	closed   bool
	sequence atomic.Uint64
}

func New(source Source, options ...Options) (*Coordinator, error) {
	if source == nil {
		return nil, errors.New("runtime observation source is required")
	}
	config := Options{}
	if len(options) > 0 {
		config = options[0]
	}
	if config.RefreshInterval <= 0 {
		config.RefreshInterval = defaultRefreshInterval
	}
	if config.FreshnessWindow <= 0 {
		config.FreshnessWindow = defaultFreshnessWindow
	}
	if config.ReleaseGrace <= 0 {
		config.ReleaseGrace = defaultReleaseGrace
	}
	if config.CollectTimeout <= 0 {
		config.CollectTimeout = defaultCollectTimeout
	}
	if config.CheckpointEvery <= 0 {
		config.CheckpointEvery = defaultCheckpointEvery
	}
	if config.RemoteConcurrency <= 0 {
		config.RemoteConcurrency = 2
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Broker == nil {
		config.Broker = NewBroker()
	}
	return &Coordinator{
		source: source, broker: config.Broker, now: config.Now,
		refreshInterval: config.RefreshInterval, freshnessWindow: config.FreshnessWindow,
		releaseGrace: config.ReleaseGrace, collectTimeout: config.CollectTimeout,
		checkpointEvery: config.CheckpointEvery,
		remoteSlots:     make(chan struct{}, config.RemoteConcurrency), localSlots: make(chan struct{}, 1),
		cache: make(map[string]cacheEntry), flights: make(map[string]*flight), leases: make(map[string]*lease),
	}, nil
}

func endpointKey(targetID, installationID string) string {
	return strings.TrimSpace(targetID) + "\x00" + strings.TrimSpace(installationID)
}

func scopeKey(scope Scope) string {
	return strings.TrimSpace(scope.TargetID) + "\x00" + strings.TrimSpace(scope.RoomID)
}

func installationID(item agents.RuntimeTargetInventory) string {
	return strings.TrimSpace(item.Target.Config.InstallationID)
}

func (c *Coordinator) RuntimeTargetInventories(ctx context.Context) ([]agents.RuntimeTargetInventory, error) {
	base, err := c.source.CachedRuntimeTargetInventories(ctx)
	if err != nil {
		return nil, err
	}
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]agents.RuntimeTargetInventory, 0, len(base))
	for _, current := range base {
		entry, exists := c.cache[endpointKey(current.Target.ID, installationID(current))]
		if exists && entry.inventory.Available {
			hot := entry.inventory
			hot.Target = current.Target
			current = hot
		}
		state, message, started := c.projectState(now, current, entry, exists)
		current.ObservationState = string(state)
		current.ObservationError = message
		current.RefreshStartedAt = started
		current.Stale = state != StateFresh && state != StateRefreshing
		switch state {
		case StateOffline:
			current.StaleReason = "agent_offline"
		case StateUnavailable:
			if current.StaleReason == "" {
				current.StaleReason = "runtime_not_configured"
			}
		case StateError:
			current.StaleReason = "collection_failed"
		case StateStale:
			if current.StaleReason == "" {
				current.StaleReason = "report_expired"
			}
		default:
			current.StaleReason = ""
		}
		items = append(items, current)
	}
	return items, nil
}

func (c *Coordinator) projectState(now time.Time, item agents.RuntimeTargetInventory, entry cacheEntry, exists bool) (State, string, *time.Time) {
	if !item.Target.Configured {
		return StateUnavailable, "运行目标尚未配置", nil
	}
	if !item.Target.Online {
		return StateOffline, "运行目标离线", nil
	}
	if exists && entry.state == StateRefreshing {
		return StateRefreshing, entry.err, entry.refreshStarted
	}
	if exists && entry.state == StateError {
		return StateError, entry.err, nil
	}
	if !item.Available || item.ReceivedAt == nil || item.ReceivedAt.IsZero() {
		return StateUnavailable, "尚未采集运行目标清单", nil
	}
	if now.Sub(item.ReceivedAt.UTC()) > c.freshnessWindow {
		return StateStale, "运行目标清单已过期", nil
	}
	return StateFresh, "", nil
}

func (c *Coordinator) Snapshot(ctx context.Context, scope Scope) ([]Observation, error) {
	items, err := c.RuntimeTargetInventories(ctx)
	if err != nil {
		return nil, err
	}
	observations := make([]Observation, 0, len(items))
	foundTarget := strings.TrimSpace(scope.TargetID) == ""
	for _, item := range items {
		if scope.TargetID != "" && item.Target.ID != scope.TargetID {
			continue
		}
		foundTarget = true
		observations = append(observations, observationFromInventory(item))
	}
	if !foundTarget {
		return nil, ErrRuntimeTargetNotFound
	}
	sort.Slice(observations, func(i, j int) bool {
		if observations[i].TargetID == observations[j].TargetID {
			return observations[i].InstallationID < observations[j].InstallationID
		}
		return observations[i].TargetID < observations[j].TargetID
	})
	return observations, nil
}

func observationFromInventory(item agents.RuntimeTargetInventory) Observation {
	return Observation{
		TargetID: item.Target.ID, TargetName: item.Target.Name,
		InstallationID: installationID(item), State: State(item.ObservationState),
		ObservedAt: item.ObservedAt, ReceivedAt: item.ReceivedAt,
		RefreshStartedAt: item.RefreshStartedAt, Error: item.ObservationError,
	}
}

func (c *Coordinator) Refresh(ctx context.Context, scope Scope) ([]RefreshResult, error) {
	items, err := c.source.CachedRuntimeTargetInventories(ctx)
	if err != nil {
		return nil, err
	}
	foundTarget := strings.TrimSpace(scope.TargetID) == ""
	selected := make([]agents.RuntimeTargetInventory, 0, len(items))
	for _, item := range items {
		if scope.TargetID != "" && item.Target.ID != scope.TargetID {
			continue
		}
		foundTarget = true
		selected = append(selected, item)
	}
	if !foundTarget {
		return nil, ErrRuntimeTargetNotFound
	}
	results := make([]RefreshResult, len(selected))
	var wait sync.WaitGroup
	for index, item := range selected {
		index, item := index, item
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index] = c.refreshEndpoint(ctx, item)
		}()
	}
	wait.Wait()
	sort.Slice(results, func(i, j int) bool {
		if results[i].TargetID == results[j].TargetID {
			return results[i].InstallationID < results[j].InstallationID
		}
		return results[i].TargetID < results[j].TargetID
	})
	return results, nil
}

// RefreshRuntimeTarget forces one endpoint observation and waits for the
// result. Configuration publication uses this boundary to prove that a
// target-specific file change is visible before lifecycle preflight runs.
func (c *Coordinator) RefreshRuntimeTarget(ctx context.Context, targetID, targetInstallationID string) error {
	targetID = strings.TrimSpace(targetID)
	targetInstallationID = strings.TrimSpace(targetInstallationID)
	if targetID == "" || targetInstallationID == "" {
		return ErrRuntimeTargetNotFound
	}
	items, err := c.source.CachedRuntimeTargetInventories(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Target.ID != targetID || installationID(item) != targetInstallationID {
			continue
		}
		result := c.refreshEndpoint(ctx, item)
		if result.State != StateFresh {
			return &UnavailableError{
				TargetID: result.TargetID, InstallationID: result.InstallationID,
				State: result.State, Message: result.Error,
			}
		}
		return nil
	}
	return ErrRuntimeTargetNotFound
}

func (c *Coordinator) refreshEndpoint(ctx context.Context, item agents.RuntimeTargetInventory) RefreshResult {
	key := endpointKey(item.Target.ID, installationID(item))
	c.mu.Lock()
	if existing := c.flights[key]; existing != nil {
		existing.waiters++
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return refreshResult(item, StateError, ctx.Err().Error())
		case <-existing.done:
			return existing.result
		}
	}
	current := c.cache[key]
	if !current.inventory.Available && item.Available {
		current.inventory = item
	}
	previous := current
	started := c.now().UTC()
	current.state, current.err, current.refreshStarted = StateRefreshing, "", &started
	c.cache[key] = current
	running := &flight{done: make(chan struct{})}
	c.flights[key] = running
	c.mu.Unlock()
	c.publish("observation.refreshing", observationFromEntry(item, current))

	result := c.collect(ctx, item, key, previous)
	c.mu.Lock()
	running.result = result
	delete(c.flights, key)
	close(running.done)
	c.mu.Unlock()
	return result
}

func (c *Coordinator) collect(ctx context.Context, item agents.RuntimeTargetInventory, key string, previous cacheEntry) RefreshResult {
	if !item.Target.Configured {
		return c.finishError(item, key, StateUnavailable, "运行目标尚未配置", previous)
	}
	if !item.Target.Online {
		return c.finishError(item, key, StateOffline, "运行目标离线", previous)
	}
	slots := c.remoteSlots
	if item.Target.ID == "local" {
		slots = c.localSlots
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return c.finishError(item, key, StateError, ctx.Err().Error(), previous)
	}
	collectContext, cancel := context.WithTimeout(ctx, c.collectTimeout)
	observed, err := c.source.CollectRuntimeTargetInventory(collectContext, item.Target.ID, installationID(item))
	cancel()
	if err != nil {
		state := StateError
		if !item.Target.Online {
			state = StateOffline
		}
		return c.finishError(item, key, state, err.Error(), previous)
	}
	observed.ObservationState = string(StateFresh)
	c.mu.Lock()
	entry := c.cache[key]
	entry.inventory, entry.state, entry.err, entry.refreshStarted = observed, StateFresh, "", nil
	digest := structuralDigest(observed.Inventory)
	checkpoint := observed.Target.ID != "local" && (entry.lastCheckpoint.IsZero() || entry.checkpointHash != digest || c.now().UTC().Sub(entry.lastCheckpoint) >= c.checkpointEvery)
	c.cache[key] = entry
	c.mu.Unlock()
	observation := observationFromInventory(observed)
	// A read-triggered refresh must not notify views to perform the same read
	// again unless the inventory or its availability actually changed.
	eventType := "observation.refreshed"
	if previous.state != StateFresh || structuralDigest(previous.inventory.Inventory) != digest {
		eventType = "inventory.updated"
	}
	c.publish(eventType, observation)
	if checkpoint {
		if err := c.source.CheckpointRuntimeTargetInventory(observed.Target.ID, observed.Inventory); err != nil {
			warning := observation
			warning.Error = "运行清单检查点写入失败: " + err.Error()
			c.publish("observation.refreshed", warning)
		} else {
			c.mu.Lock()
			entry = c.cache[key]
			entry.lastCheckpoint, entry.checkpointHash = c.now().UTC(), digest
			c.cache[key] = entry
			c.mu.Unlock()
		}
	}
	return RefreshResult{
		TargetID: observed.Target.ID, TargetName: observed.Target.Name,
		InstallationID: installationID(observed), State: StateFresh, ObservedAt: observed.ObservedAt,
	}
}

func (c *Coordinator) finishError(item agents.RuntimeTargetInventory, key string, state State, message string, previous cacheEntry) RefreshResult {
	c.mu.Lock()
	entry := c.cache[key]
	if !entry.inventory.Available && item.Available {
		entry.inventory = item
	}
	entry.state, entry.err, entry.refreshStarted = state, message, nil
	c.cache[key] = entry
	c.mu.Unlock()
	observation := observationFromEntry(item, entry)
	eventType := "observation.refreshed"
	if previous.state != state || previous.err != message {
		eventType = "observation.error"
	}
	c.publish(eventType, observation)
	return refreshResult(item, state, message)
}

func refreshResult(item agents.RuntimeTargetInventory, state State, message string) RefreshResult {
	return RefreshResult{
		TargetID: item.Target.ID, TargetName: item.Target.Name,
		InstallationID: installationID(item), State: state, ObservedAt: item.ObservedAt, Error: message,
	}
}

func observationFromEntry(item agents.RuntimeTargetInventory, entry cacheEntry) Observation {
	if entry.inventory.Available {
		item = entry.inventory
	}
	return Observation{
		TargetID: item.Target.ID, TargetName: item.Target.Name, InstallationID: installationID(item),
		State: entry.state, ObservedAt: item.ObservedAt, ReceivedAt: item.ReceivedAt,
		RefreshStartedAt: entry.refreshStarted, Error: entry.err,
	}
}

func (c *Coordinator) publish(eventType string, observation Observation) {
	c.broker.Publish(Event{
		Sequence: c.sequence.Add(1), Type: eventType,
		OccurredAt: c.now().UTC(), Observation: observation,
	})
}

func structuralDigest(report shared.RuntimeInventoryReport) string {
	type processIdentity struct {
		PID             int32  `json:"pid"`
		RuntimeKind     string `json:"runtimeKind"`
		InstanceID      string `json:"instanceId"`
		Executable      string `json:"executable"`
		Cluster         string `json:"cluster"`
		Shard           string `json:"shard"`
		StorageRoot     string `json:"storageRoot"`
		ConfigDirectory string `json:"configDirectory"`
	}
	processes := make([]processIdentity, 0, len(report.Processes))
	for _, process := range report.Processes {
		processes = append(processes, processIdentity{
			PID: process.PID, RuntimeKind: process.RuntimeKind, InstanceID: process.InstanceID,
			Executable: process.Executable, Cluster: process.Cluster, Shard: process.Shard,
			StorageRoot: process.StorageRoot, ConfigDirectory: process.ConfigDirectory,
		})
	}
	payload, _ := json.Marshal(struct {
		Installation shared.RuntimeInstallationReport `json:"installation"`
		Rooms        []shared.RoomInventoryReport     `json:"rooms"`
		Processes    []processIdentity                `json:"processes"`
	}{report.Installation, report.Rooms, processes})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (c *Coordinator) Subscribe() (<-chan Event, func()) { return c.broker.Subscribe() }

// EnsureRuntimeTargetsFresh is the operation preflight boundary. It waits for
// an existing endpoint flight or actively refreshes stale targets, then fails
// closed when any selected endpoint still cannot be trusted.
func (c *Coordinator) EnsureRuntimeTargetsFresh(ctx context.Context, targetIDs []string) error {
	seen := make(map[string]bool, len(targetIDs))
	for _, targetID := range targetIDs {
		targetID = strings.TrimSpace(targetID)
		if targetID == "" || seen[targetID] {
			continue
		}
		seen[targetID] = true
		observations, err := c.Snapshot(ctx, Scope{TargetID: targetID})
		if err != nil {
			return err
		}
		fresh := len(observations) > 0
		for _, observation := range observations {
			fresh = fresh && observation.State == StateFresh
		}
		if fresh {
			continue
		}
		results, err := c.Refresh(ctx, Scope{TargetID: targetID})
		if err != nil {
			return err
		}
		for _, result := range results {
			if result.State != StateFresh {
				return &UnavailableError{
					TargetID: result.TargetID, InstallationID: result.InstallationID,
					State: result.State, Message: result.Error,
				}
			}
		}
	}
	return nil
}

// RuntimeTargetChanged schedules a non-blocking post-mutation observation.
// Endpoint singleflight makes repeated world operations collapse naturally.
func (c *Coordinator) RuntimeTargetChanged(targetID string) {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.collectTimeout)
		defer cancel()
		results, _ := c.Refresh(ctx, Scope{TargetID: targetID})
		for _, result := range results {
			if result.State != StateFresh {
				continue
			}
			// A completed operation can change readiness or files without changing
			// the inventory's process identities or room configuration fields.
			c.mu.Lock()
			entry := c.cache[endpointKey(result.TargetID, result.InstallationID)]
			observation := observationFromEntry(entry.inventory, entry)
			c.mu.Unlock()
			c.publish("topology.changed", observation)
		}
	}()
}

func (c *Coordinator) Acquire(ctx context.Context, scope Scope) (func(), error) {
	if _, err := c.Snapshot(ctx, scope); err != nil {
		return nil, err
	}
	key := scopeKey(scope)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, context.Canceled
	}
	if existing := c.leases[key]; existing != nil {
		if existing.timer != nil {
			existing.timer.Stop()
			existing.timer = nil
		}
		existing.refs++
		c.mu.Unlock()
		return c.releaseFunc(key), nil
	}
	workerContext, cancel := context.WithCancel(context.Background())
	c.leases[key] = &lease{refs: 1, cancel: cancel}
	c.mu.Unlock()
	go c.runLease(workerContext, scope)
	return c.releaseFunc(key), nil
}

func (c *Coordinator) releaseFunc(key string) func() {
	var once sync.Once
	return func() { once.Do(func() { c.release(key) }) }
}

func (c *Coordinator) release(key string) {
	c.mu.Lock()
	current := c.leases[key]
	if current == nil {
		c.mu.Unlock()
		return
	}
	current.refs--
	if current.refs > 0 {
		c.mu.Unlock()
		return
	}
	current.timer = time.AfterFunc(c.releaseGrace, func() {
		c.mu.Lock()
		leased := c.leases[key]
		if leased == current && leased.refs == 0 {
			delete(c.leases, key)
			leased.cancel()
		}
		c.mu.Unlock()
	})
	c.mu.Unlock()
}

func (c *Coordinator) runLease(ctx context.Context, scope Scope) {
	_, _ = c.Refresh(ctx, scope)
	ticker := time.NewTicker(c.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = c.Refresh(ctx, scope)
		}
	}
}

func (c *Coordinator) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for key, current := range c.leases {
		if current.timer != nil {
			current.timer.Stop()
		}
		current.cancel()
		delete(c.leases, key)
	}
	c.mu.Unlock()
}
