package containers

import (
	"context"
	"sort"
	"sync"
)

type MemoryTransport struct {
	mu    sync.Mutex
	items map[string]Container
}

func NewMemoryTransport() *MemoryTransport {
	return &MemoryTransport{items: map[string]Container{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "dstserver-surface", Image: "dstserver:latest", Command: "./start.sh", State: "running", Status: "Up 2 hours", Ports: "10999/udp", Labels: map[string]string{"com.dst-admin.managed": "true"}, Running: true, Managed: true, CreatedAt: "2026-08-08 07:00:00 +0800 CST"},
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": {ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Name: "dstserver-caves", Image: "dstserver:latest", Command: "./start.sh", State: "exited", Status: "Exited (0) 1 hour ago", Ports: "11000/udp", Labels: map[string]string{"com.dst-admin.managed": "true"}, Running: false, Managed: true, CreatedAt: "2026-08-08 07:00:00 +0800 CST"},
	}}
}

func (t *MemoryTransport) Available() bool { return true }

func (t *MemoryTransport) List(context.Context) ([]Container, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	items := make([]Container, 0, len(t.items))
	for _, item := range t.items {
		item.Labels = cloneLabels(item.Labels)
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (t *MemoryTransport) Run(_ context.Context, id string, action Action) (ActionResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	item, exists := t.items[id]
	if !exists {
		return ActionResult{}, ErrNotFound
	}
	switch action {
	case ActionStart:
		item.Running, item.State, item.Status = true, "running", "Up 1 second"
	case ActionStop:
		item.Running, item.State, item.Status = false, "exited", "Exited (0) 1 second ago"
	case ActionRestart:
		item.Running, item.State, item.Status = true, "running", "Up 1 second"
	case ActionRemove:
		delete(t.items, id)
		return ActionResult{Output: item.Name}, nil
	default:
		return ActionResult{}, ErrInvalidInput
	}
	t.items[id] = item
	return ActionResult{Output: item.Name}, nil
}

func cloneLabels(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
