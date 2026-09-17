package runtimeobservation

import "sync"

type Broker struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[int]chan Event
}

func NewBroker() *Broker {
	return &Broker{subscribers: make(map[int]chan Event)}
}

func (b *Broker) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	updates := make(chan Event, 16)
	b.subscribers[id] = updates
	b.mu.Unlock()
	return updates, func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
	}
}

func (b *Broker) Publish(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, subscriber := range b.subscribers {
		select {
		case subscriber <- event:
		default:
			// The next event carries the complete endpoint state, so a slow
			// subscriber may safely coalesce intermediate transitions.
		}
	}
}
