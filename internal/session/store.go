package session

import (
	"sync"
	"time"
)

const ringBufferSize = 50 * 1024 // 50KB

// RingBuffer is a fixed-size circular buffer for storing terminal output.
type RingBuffer struct {
	buf  []byte
	size int
	pos  int
	full bool
	mu   sync.Mutex
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]byte, size), size: size}
}

func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.size {
		copy(r.buf, p[n-r.size:])
		r.pos = 0
		r.full = true
		return n, nil
	}
	if r.pos+n <= r.size {
		copy(r.buf[r.pos:], p)
	} else {
		first := r.size - r.pos
		copy(r.buf[r.pos:], p[:first])
		copy(r.buf, p[first:])
	}
	r.pos = (r.pos + n) % r.size
	if !r.full && r.pos < n {
		r.full = true
	}
	return n, nil
}

func (r *RingBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]byte(nil), r.buf[:r.pos]...)
	}
	out := make([]byte, r.size)
	copy(out, r.buf[r.pos:])
	copy(out[r.size-r.pos:], r.buf[:r.pos])
	return out
}

type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Output    *RingBuffer `json:"-"`
}

type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	order    []string
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]*Session)}
}

func (s *Store) Create(id, name string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &Session{
		ID:        id,
		Name:      name,
		CreatedAt: time.Now(),
		Output:    NewRingBuffer(ringBufferSize),
	}
	s.sessions[id] = sess
	s.order = append(s.order, id)
	return sess
}

func (s *Store) Get(id string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.sessions[id])
	}
	return out
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return false
	}
	delete(s.sessions, id)
	for i, oid := range s.order {
		if oid == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}
