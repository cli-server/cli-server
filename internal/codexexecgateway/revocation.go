package codexexecgateway

import (
	"container/list"
	"sync"
	"time"
)

// RevokedSet is a bounded, concurrent-safe set of revoked turn_ids with
// per-entry expiry. Designed for the spec's "in-memory revoked set, cap
// ~10k, periodically pruned of entries past their original exp".
//
// RevokedSet is bounded; when at cap, Add evicts the oldest entry by
// insertion order, NOT by expiry. If the evicted entry's exp is still
// in the future, that turn_id is silently un-revoked (its token will
// pass Contains as if never revoked, until its own exp). Add returns
// a bool indicating this case so callers can log/alert.
type RevokedSet struct {
	mu    sync.Mutex
	cap   int
	order *list.List               // FIFO of turn_ids; front = oldest
	items map[string]*list.Element // turn_id → element holding {turnID, exp}
}

type revokedEntry struct {
	turnID string
	exp    int64 // unix seconds
}

func NewRevokedSet(cap int) *RevokedSet {
	if cap <= 0 {
		cap = 10000
	}
	return &RevokedSet{
		cap:   cap,
		order: list.New(),
		items: make(map[string]*list.Element, cap),
	}
}

// Add inserts (turnID, exp). Re-adding refreshes the entry's position.
// When at capacity, the oldest entry is evicted. Returns evictedLive=true
// if the evicted entry's exp was still in the future — callers should log a
// warning because that turn_id is no longer blocked by the revoked set.
func (r *RevokedSet) Add(turnID string, exp int64) (evictedLive bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.items[turnID]; ok {
		el.Value = revokedEntry{turnID: turnID, exp: exp}
		r.order.MoveToBack(el)
		return false
	}
	now := time.Now().Unix()
	for r.order.Len() >= r.cap {
		oldest := r.order.Front()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(revokedEntry)
		r.order.Remove(oldest)
		delete(r.items, entry.turnID)
		if entry.exp > now {
			evictedLive = true
		}
	}
	el := r.order.PushBack(revokedEntry{turnID: turnID, exp: exp})
	r.items[turnID] = el
	return evictedLive
}

// Contains reports whether turnID is in the set (regardless of expiry —
// callers may rely on Prune to clean stale entries).
func (r *RevokedSet) Contains(turnID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.items[turnID]
	return ok
}

// Prune removes entries whose exp has passed.
func (r *RevokedSet) Prune() {
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	for el := r.order.Front(); el != nil; {
		next := el.Next()
		if el.Value.(revokedEntry).exp < now {
			r.order.Remove(el)
			delete(r.items, el.Value.(revokedEntry).turnID)
		}
		el = next
	}
}

// Size returns the current number of entries.
func (r *RevokedSet) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order.Len()
}

// StartPruner runs Prune at the given interval until stop is closed.
// Caller is responsible for closing stop on shutdown.
func (r *RevokedSet) StartPruner(stop <-chan struct{}, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.Prune()
			}
		}
	}()
}
