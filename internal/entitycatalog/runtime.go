package entitycatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dont/internal/dstruntime"
	"github.com/google/uuid"
)

var ErrRuntimeCatalogUnavailable = errors.New("world entity catalog is unavailable")
var ErrRuntimeCatalogLimit = errors.New("world entity catalog exceeded its collection budget")

const snapshotPageLimit = 2048

var errSnapshotBatchUnsupported = errors.New("runtime does not support catalog batches")

type RuntimeCommander interface {
	ExecuteCommand(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error)
}

type RuntimeSearchOptions struct {
	Query   string `json:"query"`
	Source  string `json:"source"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit"`
	Refresh bool   `json:"refresh"`
}

type RuntimeMod struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type RuntimeSearchResult struct {
	Items      []Entity     `json:"items"`
	Mods       []RuntimeMod `json:"mods"`
	Total      int          `json:"total"`
	Offset     int          `json:"offset"`
	HasMore    bool         `json:"hasMore"`
	ObservedAt time.Time    `json:"observedAt"`
}

// SearchRuntime reads registered prefabs without spawning entities or loading mod
// files. The existing bridge resolves applied placement for local and Agent worlds.
func SearchRuntime(ctx context.Context, commander RuntimeCommander, roomID, worldID string, options RuntimeSearchOptions) (RuntimeSearchResult, error) {
	return searchRuntimePage(ctx, commander, roomID, worldID, options, false)
}

func searchRuntimePage(ctx context.Context, commander RuntimeCommander, roomID, worldID string, options RuntimeSearchOptions, snapshot bool) (RuntimeSearchResult, error) {
	maximum := 120
	if snapshot {
		maximum = snapshotPageLimit
	}
	options.Query = strings.TrimSpace(options.Query)
	if len([]rune(options.Query)) > 100 || options.Offset < 0 || options.Offset > 50000 || options.Limit < 1 || options.Limit > maximum || len(options.Source) > 128 {
		return RuntimeSearchResult{}, ErrInvalidSearch
	}
	if commander == nil {
		return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
	}
	receipt, err := commander.ExecuteCommand(ctx, roomID, worldID, dstruntime.CommandRequest{
		RequestID: uuid.NewString(), Action: "catalog.entities", Arguments: map[string]interface{}{"query": options.Query, "source": options.Source, "offset": options.Offset, "limit": options.Limit, "refresh": options.Refresh, "snapshot": snapshot},
	})
	if err != nil {
		return RuntimeSearchResult{}, fmt.Errorf("%w: %w", ErrRuntimeCatalogUnavailable, err)
	}
	if !receipt.OK {
		if snapshot && receipt.Code == "INVALID_SEARCH" {
			return RuntimeSearchResult{}, errSnapshotBatchUnsupported
		}
		if receipt.Code == "CATALOG_TOO_LARGE" || receipt.Code == "CATALOG_TIMEOUT" {
			return RuntimeSearchResult{}, ErrRuntimeCatalogLimit
		}
		return RuntimeSearchResult{}, fmt.Errorf("%w: %s", ErrRuntimeCatalogUnavailable, receipt.Code)
	}
	data, err := json.Marshal(receipt.Details)
	if err != nil {
		return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
	}
	// Klei's JSON encoder emits {} for an empty Lua array. Normalize those two
	// collection fields before decoding, while rejecting malformed nonempty data.
	var wire map[string]json.RawMessage
	if json.Unmarshal(data, &wire) != nil {
		return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
	}
	for _, key := range []string{"items", "mods"} {
		if string(wire[key]) == "{}" {
			wire[key] = json.RawMessage("[]")
		}
	}
	data, _ = json.Marshal(wire)
	var result RuntimeSearchResult
	if json.Unmarshal(data, &result) != nil || len(result.Items) > options.Limit || result.Total < 0 || result.Offset != options.Offset {
		return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
	}
	for index := range result.Items {
		item := &result.Items[index]
		if !prefabPattern.MatchString(item.ID) {
			return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
		}
		item.Key = "runtime:" + item.ID
		item.Namespace = "runtime"
		item.Source = "runtime"
		// Registration alone cannot prove component support. Execution checks the
		// actual entity; catalog collection never instantiates it to guess its type.
		item.Type = "unknown"
		item.Capabilities = []Capability{CapabilityGive, CapabilitySpawn, CapabilityRemove}
	}
	if result.Items == nil {
		result.Items = []Entity{}
	}
	if result.Mods == nil {
		result.Mods = []RuntimeMod{}
	}
	if result.ObservedAt.IsZero() {
		result.ObservedAt = receipt.CompletedAt
	}
	return result, nil
}
