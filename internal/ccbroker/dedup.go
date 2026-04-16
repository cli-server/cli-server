package ccbroker

import "sync"

// BoundedUUIDSet is a fixed-capacity set with FIFO eviction.
// Used for echo/replay deduplication, matching CC's BoundedUUIDSet (capacity 2000).
// Thread-safe.
type BoundedUUIDSet struct {
	mu       sync.Mutex
	capacity int
	set      map[string]struct{}
	ring     []string
	idx      int
	count    int
}

// NewBoundedUUIDSet creates a new bounded set with the given capacity.
func NewBoundedUUIDSet(capacity int) *BoundedUUIDSet {
	return &BoundedUUIDSet{
		capacity: capacity,
		set:      make(map[string]struct{}, capacity),
		ring:     make([]string, capacity),
	}
}

// Add inserts a UUID. Returns false if already present (duplicate).
func (s *BoundedUUIDSet) Add(uuid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.set[uuid]; exists {
		return false
	}
	if s.count >= s.capacity {
		old := s.ring[s.idx]
		delete(s.set, old)
	} else {
		s.count++
	}
	s.ring[s.idx] = uuid
	s.set[uuid] = struct{}{}
	s.idx = (s.idx + 1) % s.capacity
	return true
}

// DedupRegistry maintains per-session deduplication sets.
type DedupRegistry struct {
	mu       sync.Mutex
	sessions map[string]*BoundedUUIDSet
}

// NewDedupRegistry creates a new dedup registry.
func NewDedupRegistry() *DedupRegistry {
	return &DedupRegistry{
		sessions: make(map[string]*BoundedUUIDSet),
	}
}

// GetOrCreate returns the dedup set for a session, creating one if needed.
func (r *DedupRegistry) GetOrCreate(sessionID string) *BoundedUUIDSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[sessionID]; ok {
		return s
	}
	s := NewBoundedUUIDSet(2000)
	r.sessions[sessionID] = s
	return s
}
