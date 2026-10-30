package dstruntime

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"dont/internal/runtimefiles"
	"dont/shared"
)

// These templates are control-plane assets, not installable/custom commands.
//
//go:embed queries/*.lua
var queryTemplates embed.FS

var queryPrefab = regexp.MustCompile(`^[A-Za-z0-9_]{1,80}$`)

type WorldQueryRequest struct {
	RequestID string
	QueryID   string
	Prefab    string
	// ExpectedIdentity fences cached observations and late placement changes.
	ExpectedIdentity string
}

func ValidateWorldQuery(queryID, prefab string) error {
	switch queryID {
	case "world_info", "online_players":
		if prefab == "" {
			return nil
		}
	case "entity_count":
		if queryPrefab.MatchString(prefab) {
			return nil
		}
	}
	return fmt.Errorf("%w: unknown query or invalid parameters", ErrRuntimeRequestInvalid)
}

func worldQueryCommand(q WorldQueryRequest) (CommandRequest, error) {
	if q.ExpectedIdentity == "" {
		return CommandRequest{}, ErrRuntimeRequestInvalid
	}
	if err := ValidateWorldQuery(q.QueryID, q.Prefab); err != nil {
		return CommandRequest{}, err
	}
	if !runtimeRequestIDPattern.MatchString(q.RequestID) {
		return CommandRequest{}, ErrRuntimeRequestInvalid
	}
	common, err := queryTemplates.ReadFile("queries/common.lua")
	if err != nil {
		return CommandRequest{}, err
	}
	body, err := queryTemplates.ReadFile("queries/" + q.QueryID + ".lua")
	if err != nil {
		return CommandRequest{}, err
	}
	script := "local request_id=" + quoteRuntimeLua(q.RequestID) + ";local query_id=" + quoteRuntimeLua(q.QueryID) + ";local prefab=" + quoteRuntimeLua(q.Prefab) + ";\n" + string(common) + string(body)
	if len(script) > 4096 {
		return CommandRequest{}, ErrRuntimeRequestInvalid
	}
	return CommandRequest{RequestID: q.RequestID, Action: "console.execute", Arguments: map[string]interface{}{"script": script}}, nil
}

func queryIdentity(h Health) string {
	identity, _ := json.Marshal([]string{h.SessionID, h.ShardID, h.ProducerInstanceID, h.Modules["commands"].ProducerInstanceID})
	return fmt.Sprintf("%x", sha256.Sum256(identity))
}

func readyQueryModule(h Health, now time.Time) error {
	m := h.Modules["commands"]
	if h.ProducerVersion != RuntimeVersion || !h.Running || !h.Ready || !h.FreshAt(now) || !m.Running || !m.Ready {
		return ErrRuntimeUnavailable
	}
	if m.Busy {
		return ErrRuntimeCommandBusy
	}
	return nil
}

// QueryIdentity observes current process identity without refreshing/installing
// anything. The Placement-aware artifact reader never falls back to local.
func (b *DistributedBridge) QueryIdentity(ctx context.Context, roomID, worldID string) (string, error) {
	if err := b.requireRunning(ctx, roomID, worldID); err != nil {
		return "", err
	}
	h, err := b.Health(ctx, roomID, worldID)
	if err != nil {
		return "", err
	}
	current, err := b.currentProcessHealth(ctx, roomID, worldID, h)
	if err != nil {
		return "", err
	}
	if !current || !h.Running || !h.Ready || !h.FreshAt(b.now().UTC()) {
		return "", ErrRuntimeUnavailable
	}
	placement := ""
	if reader, ok := b.runtime.(interface {
		QueryPlacementIdentity(context.Context, string, string) (string, error)
	}); ok {
		placement, err = reader.QueryPlacementIdentity(ctx, roomID, worldID)
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x|%s", sha256.Sum256([]byte(placement)), queryIdentity(h)), nil
}

func queryHealthMatches(expected string, h Health) bool {
	_, identity, ok := strings.Cut(expected, "|")
	if !ok {
		identity = expected
	}
	return identity == queryIdentity(h)
}

func (b *DistributedBridge) ExecuteWorldQuery(ctx context.Context, roomID, worldID string, q WorldQueryRequest) (CommandReceipt, error) {
	r, err := worldQueryCommand(q)
	if err != nil {
		return CommandReceipt{}, err
	}
	return b.executeCommand(ctx, roomID, worldID, r, q.ExpectedIdentity)
}

func (b *Bridge) ExecuteWorldQuery(ctx context.Context, roomID, worldID string, q WorldQueryRequest) (CommandReceipt, error) {
	r, err := worldQueryCommand(q)
	if err != nil {
		return CommandReceipt{}, err
	}
	return b.executeCommand(ctx, roomID, worldID, r, q.ExpectedIdentity)
}

func (b *Bridge) queryReady(ctx context.Context, roomID, worldID, expected string) error {
	room, world, _, err := b.resolveWorld(roomID, worldID)
	if err != nil {
		return err
	}
	running, err := b.process.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
	if err != nil {
		return err
	}
	if !running {
		return ErrRuntimeUnavailable
	}
	h, err := b.manager.Health(roomID, worldID)
	if err != nil {
		return err
	}
	current, err := runtimefiles.ReadWorldIdentity(ctx, b.manager.root, room.DirectoryName, world.DirectoryName, shared.ShardRuntimeStatus{State: "running"})
	if err != nil {
		return err
	}
	if !healthMatchesCurrentProcess(b.now().UTC(), h, current) || queryIdentity(h) != expected {
		return ErrRuntimeUnavailable
	}
	return readyQueryModule(h, b.now().UTC())
}
