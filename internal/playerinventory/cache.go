package playerinventory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"dont/shared"
)

type cachedSave struct {
	data []byte
	at   time.Time
}

var savedResults = struct {
	sync.Mutex
	items map[[32]byte]cachedSave
	bytes int
}{items: map[[32]byte]cachedSave{}}

// Only parsed item data is cached. File bytes and the selected index/path are
// re-read and verified on every access, including same-size/mtime rewrites.
func normalizeCached(ctx context.Context, data []byte, result *shared.PlayerInventory, refresh bool) error {
	key := sha256.Sum256(data)
	now := time.Now()
	savedResults.Lock()
	cached, ok := savedResults.items[key]
	savedResults.Unlock()
	if !refresh && ok && now.Sub(cached.at) < 10*time.Second {
		var value shared.PlayerInventory
		if json.Unmarshal(cached.data, &value) == nil {
			copyItems(result, value)
			result.CacheAgeMS = now.Sub(cached.at).Milliseconds()
			return nil
		}
	}
	root, err := parse(ctx, data)
	if err != nil {
		return err
	}
	if err = normalize(root, result); err != nil {
		return err
	}
	value := shared.PlayerInventory{}
	copyItems(&value, *result)
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > 192<<10 {
		return failure("SAVE_LIMIT_EXCEEDED")
	}
	savedResults.Lock()
	defer savedResults.Unlock()
	if old, exists := savedResults.items[key]; exists {
		savedResults.bytes -= len(old.data)
		delete(savedResults.items, key)
	}
	for len(savedResults.items) >= 32 || savedResults.bytes+len(encoded) > 8<<20 {
		var oldest [32]byte
		var at time.Time
		for k, v := range savedResults.items {
			if at.IsZero() || v.at.Before(at) {
				oldest, at = k, v.at
			}
		}
		entry := savedResults.items[oldest]
		savedResults.bytes -= len(entry.data)
		delete(savedResults.items, oldest)
	}
	savedResults.items[key] = cachedSave{encoded, now}
	savedResults.bytes += len(encoded)
	return nil
}
func copyItems(to *shared.PlayerInventory, from shared.PlayerInventory) {
	to.Character, to.Status, to.Coverage, to.Empty = from.Character, from.Status, from.Coverage, from.Empty
	to.Equipment, to.Inventory, to.ActiveItem, to.SlotCount = from.Equipment, from.Inventory, from.ActiveItem, from.SlotCount
	to.Warnings, to.LimitsReached = from.Warnings, from.LimitsReached
}
