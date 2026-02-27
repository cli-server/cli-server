package session

import (
	"sync"
	"time"

	"github.com/imryao/cli-server/internal/db"
)

// Session represents a session with in-memory output buffer.
type Session struct {
	ID               string     `json:"id"`
	UserID           string     `json:"userId"`
	Name             string     `json:"name"`
	Status           string     `json:"status"`
	SandboxName      string     `json:"sandboxName,omitempty"`
	PodIP            string     `json:"podIp,omitempty"`
	OpencodePassword string     `json:"-"`
	ProxyToken       string     `json:"-"`
	CreatedAt        time.Time  `json:"createdAt"`
	LastActivityAt   *time.Time `json:"lastActivityAt,omitempty"`
	PausedAt         *time.Time `json:"pausedAt,omitempty"`
	Output           *RingBuffer `json:"-"`
}

// Store manages sessions via PostgreSQL with in-memory ring buffers for active sessions.
type Store struct {
	db      *db.DB
	mu      sync.RWMutex
	buffers map[string]*RingBuffer // session ID → ring buffer (only for running sessions)
}

func NewStore(database *db.DB) *Store {
	return &Store{
		db:      database,
		buffers: make(map[string]*RingBuffer),
	}
}

// Create inserts a new session into the DB with 'creating' status (no buffer yet).
func (s *Store) Create(id, userID, name, sandboxName, opencodePassword, proxyToken string) (*Session, error) {
	if err := s.db.CreateSession(id, userID, name, sandboxName, opencodePassword, proxyToken); err != nil {
		return nil, err
	}

	now := time.Now()
	return &Session{
		ID:               id,
		UserID:           userID,
		Name:             name,
		Status:           StatusCreating,
		SandboxName:      sandboxName,
		OpencodePassword: opencodePassword,
		ProxyToken:       proxyToken,
		CreatedAt:        now,
		LastActivityAt:   &now,
	}, nil
}

// Get returns a session from DB with its in-memory buffer (if running).
func (s *Store) Get(id string) (*Session, bool) {
	dbSess, err := s.db.GetSession(id)
	if err != nil || dbSess == nil {
		return nil, false
	}
	sess := dbSessionToSession(dbSess)
	s.mu.RLock()
	if buf, ok := s.buffers[id]; ok {
		sess.Output = buf
	}
	s.mu.RUnlock()
	return sess, true
}

// GetBuffer returns the ring buffer for an active session.
func (s *Store) GetBuffer(id string) (*RingBuffer, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	buf, ok := s.buffers[id]
	return buf, ok
}

// List returns all sessions for a user from the database.
func (s *Store) List(userID string) []*Session {
	dbSessions, err := s.db.ListSessionsByUser(userID)
	if err != nil {
		return nil
	}
	out := make([]*Session, 0, len(dbSessions))
	s.mu.RLock()
	for _, ds := range dbSessions {
		sess := dbSessionToSession(ds)
		if buf, ok := s.buffers[ds.ID]; ok {
			sess.Output = buf
		}
		out = append(out, sess)
	}
	s.mu.RUnlock()
	return out
}

// UpdateStatus transitions a session to a new status.
func (s *Store) UpdateStatus(id, status string) error {
	if err := s.db.UpdateSessionStatus(id, status); err != nil {
		return err
	}
	// Manage buffer lifecycle based on status.
	switch status {
	case StatusPaused, StatusDeleting:
		s.mu.Lock()
		delete(s.buffers, id)
		s.mu.Unlock()
	case StatusRunning:
		s.mu.Lock()
		if _, ok := s.buffers[id]; !ok {
			s.buffers[id] = NewRingBuffer(ringBufferSize)
		}
		s.mu.Unlock()
	}
	return nil
}

// Delete removes a session from the DB and its in-memory buffer.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	delete(s.buffers, id)
	s.mu.Unlock()
	return s.db.DeleteSession(id)
}

// UpdateActivity records user activity on a session.
func (s *Store) UpdateActivity(id string) {
	s.db.UpdateSessionActivity(id)
}

// EnsureBuffer creates a ring buffer for a session if one doesn't exist.
func (s *Store) EnsureBuffer(id string) *RingBuffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if buf, ok := s.buffers[id]; ok {
		return buf
	}
	buf := NewRingBuffer(ringBufferSize)
	s.buffers[id] = buf
	return buf
}

func dbSessionToSession(ds *db.Session) *Session {
	sess := &Session{
		ID:        ds.ID,
		UserID:    ds.UserID,
		Name:      ds.Name,
		Status:    ds.Status,
		CreatedAt: ds.CreatedAt,
	}
	if ds.SandboxName.Valid {
		sess.SandboxName = ds.SandboxName.String
	}
	if ds.PodIP.Valid {
		sess.PodIP = ds.PodIP.String
	}
	if ds.OpencodePassword.Valid {
		sess.OpencodePassword = ds.OpencodePassword.String
	}
	if ds.ProxyToken.Valid {
		sess.ProxyToken = ds.ProxyToken.String
	}
	if ds.LastActivityAt.Valid {
		t := ds.LastActivityAt.Time
		sess.LastActivityAt = &t
	}
	if ds.PausedAt.Valid {
		t := ds.PausedAt.Time
		sess.PausedAt = &t
	}
	return sess
}
