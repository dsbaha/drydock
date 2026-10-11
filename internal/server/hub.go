package server

import (
	"encoding/json"
	"sync"
)

// Frame is one broadcast unit: card-scoped payload bytes plus the card they
// belong to ("" for global frames).
type Frame struct {
	CardID string
	Data   []byte
}

// Hub is a tiny pub/sub broadcaster for board events and per-card chat
// frames. Subscribers filter on CardID; "" receives everything.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Frame]struct{}
}

// NewHub builds a Hub.
func NewHub() *Hub {
	return &Hub{subs: map[chan Frame]struct{}{}}
}

// Subscribe returns a channel and an unsubscribe func. filterCardID == ""
// receives all frames.
func (h *Hub) Subscribe(filterCardID string) (chan Frame, func()) {
	ch := make(chan Frame, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	done := false
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !done {
			done = true
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// Broadcast marshals v and delivers it to subscribers (optionally filtered by
// cardID). Delivery is non-blocking: slow clients drop frames.
func (h *Hub) Broadcast(cardID string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- Frame{CardID: cardID, Data: data}:
		default:
		}
	}
}
