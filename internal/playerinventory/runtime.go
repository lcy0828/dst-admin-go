package playerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/shared"

	"github.com/google/uuid"
)

type Control interface {
	IsRunning(context.Context, string, string) (bool, error)
	Send(context.Context, string, string, string) error
}

type inventoryCatalog struct{ cluster, shard string }

func (c inventoryCatalog) Room(string) (rooms.Room, error) {
	return rooms.Room{ID: c.cluster, DirectoryName: c.cluster, Managed: true}, nil
}
func (c inventoryCatalog) World(string, string) (rooms.World, error) {
	return rooms.World{ID: c.shard, RoomID: c.cluster, DirectoryName: c.shard}, nil
}
func (c inventoryCatalog) Worlds(string) ([]rooms.World, error) {
	w, _ := c.World("", "")
	return []rooms.World{w}, nil
}

type flight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   shared.PlayerInventory
	err     error
}
type Reader struct {
	mu       sync.Mutex
	flights  map[string]*flight
	worlds   map[string]bool
	slots    chan struct{}
	starts   map[string]time.Time
	interval time.Duration
}

func NewReader() *Reader {
	return NewCoalescer(2, 2*time.Second)
}

func NewCoalescer(concurrency int, interval time.Duration) *Reader {
	return &Reader{flights: map[string]*flight{}, worlds: map[string]bool{}, slots: make(chan struct{}, concurrency), starts: map[string]time.Time{}, interval: interval}
}

var Default = NewReader()

// Read coalesces the whole operation, not merely command delivery. The task
// belongs to all waiters; cancellation of one browser cannot cancel another.
func (r *Reader) Read(ctx context.Context, root, cluster, shard string, q shared.PlayerInventoryRequest, control Control) (shared.PlayerInventory, error) {
	if !ValidRequest(q) || q.Source == "auto" {
		return shared.PlayerInventory{}, failure("INVALID_PLAYER_ID")
	}
	world := root + "\x00" + cluster + "\x00" + shard
	key := world + "\x00" + q.PlayerID + "\x00" + q.Source + "\x00" + q.SourceID
	return r.Do(ctx, key, world, func(taskCtx context.Context) (shared.PlayerInventory, error) {
		if q.Source == "save" {
			return ReadSave(taskCtx, root, cluster, shard, q)
		}
		return readLive(taskCtx, root, cluster, shard, q, control)
	})
}

// Do bounds and coalesces demand-driven reads. No completed live result is kept.
func (r *Reader) Do(ctx context.Context, key, world string, read func(context.Context) (shared.PlayerInventory, error)) (shared.PlayerInventory, error) {
	if err := ctx.Err(); err != nil {
		return shared.PlayerInventory{}, err
	}
	r.mu.Lock()
	f := r.flights[key]
	if f != nil && (f.waiters == 0 || f.waiters >= 32) {
		r.mu.Unlock()
		return shared.PlayerInventory{}, failure("INVENTORY_BUSY")
	}
	if f == nil {
		now := time.Now()
		if len(r.flights) >= 16 || r.worlds[world] || now.Sub(r.starts[key]) < r.interval {
			r.mu.Unlock()
			return shared.PlayerInventory{}, failure("INVENTORY_BUSY")
		}
		for k, at := range r.starts {
			if now.Sub(at) >= r.interval {
				delete(r.starts, k)
			}
		}
		if len(r.starts) >= 256 {
			r.mu.Unlock()
			return shared.PlayerInventory{}, failure("INVENTORY_BUSY")
		}
		r.starts[key] = now
		taskCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		f = &flight{done: make(chan struct{}), cancel: cancel}
		r.flights[key] = f
		r.worlds[world] = true
		go func() {
			queue := time.NewTimer(2 * time.Second)
			defer queue.Stop()
			select {
			case r.slots <- struct{}{}:
				if taskCtx.Err() != nil {
					f.err = taskCtx.Err()
				} else {
					f.value, f.err = read(taskCtx)
				}
				<-r.slots
			case <-queue.C:
				f.err = failure("INVENTORY_BUSY")
			case <-taskCtx.Done():
				f.err = taskCtx.Err()
			}
			cancel()
			r.mu.Lock()
			delete(r.flights, key)
			delete(r.worlds, world)
			close(f.done)
			r.mu.Unlock()
		}()
	}
	f.waiters++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
		}
		r.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return shared.PlayerInventory{}, ctx.Err()
	case <-f.done:
		// Callers attach their own room/world metadata; never share mutable trees.
		encoded, err := json.Marshal(f.value)
		if err != nil {
			return shared.PlayerInventory{}, err
		}
		var copy shared.PlayerInventory
		err = json.Unmarshal(encoded, &copy)
		if f.err != nil {
			return copy, f.err
		}
		return copy, err
	}
}

func readLive(ctx context.Context, root, cluster, shard string, q shared.PlayerInventoryRequest, control Control) (shared.PlayerInventory, error) {
	value := baseResult(q.PlayerID, "live")
	value.TimeSource = "captured"
	if control == nil {
		return value, failure("INVENTORY_CAPABILITY_UNAVAILABLE")
	}
	running, err := control.IsRunning(ctx, cluster, shard)
	if err != nil {
		return value, err
	}
	if !running {
		return value, failure("PLAYER_NOT_ONLINE")
	}
	manager, err := dstruntime.NewManager(root, inventoryCatalog{cluster, shard})
	if err != nil {
		return value, err
	}
	bridge, err := dstruntime.NewBridge(manager, control, control)
	if err != nil {
		return value, err
	}
	receipt, err := bridge.ExecuteCommand(ctx, cluster, shard, dstruntime.CommandRequest{RequestID: uuid.NewString(), Action: "player.inventory", Arguments: map[string]interface{}{"userId": q.PlayerID}})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return value, failure("INVENTORY_TIMEOUT")
		}
		if errors.Is(err, dstruntime.ErrRuntimeCommandBusy) {
			return value, failure("INVENTORY_BUSY")
		}
		return value, failure("INVENTORY_CAPABILITY_UNAVAILABLE")
	}
	if !receipt.OK {
		code := receipt.Code
		if code == "ACTION_NOT_ALLOWED" {
			code = "INVENTORY_CAPABILITY_UNAVAILABLE"
		}
		if code == "PLAYER_NOT_FOUND" {
			code = "PLAYER_NOT_ONLINE"
		}
		return value, failure(code)
	}
	health, healthErr := manager.Health(cluster, shard)
	if healthErr != nil || health.Modules["commands"].ProducerInstanceID != receipt.ProducerInstanceID || health.SessionID != receipt.SessionID {
		return value, failure("PLAYER_LOCATION_UNCERTAIN")
	}
	data, err := json.Marshal(receipt.Details)
	if err != nil || len(data) > 192<<10 {
		return value, failure("SAVE_LIMIT_EXCEEDED")
	}
	if err = json.Unmarshal(data, &value); err != nil || value.PlayerID != q.PlayerID || value.Source != "live" || value.SchemaVersion != 1 {
		return value, failure("SAVE_INVALID")
	}
	value.SessionID = receipt.SessionID
	value.CapturedAt = time.Unix(receipt.CompletedAtUnix, 0).UTC()
	value.TimeSource = "captured"
	return value, nil
}
