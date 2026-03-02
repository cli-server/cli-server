package sbxstore

import (
	"time"

	"github.com/agentserver/agentserver/internal/db"
)

// Sandbox represents a sandbox with its current state.
type Sandbox struct {
	ID              string     `json:"id"`
	ShortID         string     `json:"shortId,omitempty"`
	WorkspaceID     string     `json:"workspaceId"`
	Name            string     `json:"name"`
	Type            string     `json:"type"`
	Status          string     `json:"status"`
	SandboxName     string     `json:"sandboxName,omitempty"`
	PodIP           string     `json:"podIp,omitempty"`
	ProxyToken      string     `json:"-"`
	OpencodeToken   string     `json:"-"`
	OpenclawToken   string     `json:"-"`
	TunnelToken     string     `json:"-"`
	CreatedAt       time.Time  `json:"createdAt"`
	LastActivityAt  *time.Time `json:"lastActivityAt,omitempty"`
	PausedAt        *time.Time `json:"pausedAt,omitempty"`
	IsLocal         bool       `json:"isLocal"`
	LastHeartbeatAt *time.Time `json:"lastHeartbeatAt,omitempty"`
	CPU             int        `json:"cpu,omitempty"`
	Memory          int64      `json:"memory,omitempty"`
	IdleTimeout     *int       `json:"idleTimeout,omitempty"`
}

// Store manages sandboxes via PostgreSQL.
type Store struct {
	db *db.DB
}

func NewStore(database *db.DB) *Store {
	return &Store{db: database}
}

// Create inserts a new sandbox into the DB with 'creating' status.
func (s *Store) Create(id, workspaceID, name, sandboxType, sandboxName, opencodeToken, proxyToken, openclawToken, shortID string, cpu int, memory int64, idleTimeout *int) (*Sandbox, error) {
	if err := s.db.CreateSandbox(id, workspaceID, name, sandboxType, sandboxName, opencodeToken, proxyToken, openclawToken, shortID, cpu, memory, idleTimeout); err != nil {
		return nil, err
	}

	now := time.Now()
	return &Sandbox{
		ID:               id,
		ShortID:          shortID,
		WorkspaceID:      workspaceID,
		Name:             name,
		Type:             sandboxType,
		Status:           StatusCreating,
		SandboxName:      sandboxName,
		OpencodeToken: opencodeToken,
		ProxyToken:    proxyToken,
		OpenclawToken: openclawToken,
		CreatedAt:        now,
		LastActivityAt:   &now,
		CPU:              cpu,
		Memory:           memory,
		IdleTimeout:      idleTimeout,
	}, nil
}

// Get returns a sandbox from DB.
func (s *Store) Get(id string) (*Sandbox, bool) {
	dbSbx, err := s.db.GetSandbox(id)
	if err != nil || dbSbx == nil {
		return nil, false
	}
	return dbSandboxToSandbox(dbSbx), true
}

// GetByShortID returns a sandbox looked up by its short ID.
func (s *Store) GetByShortID(shortID string) (*Sandbox, bool) {
	dbSbx, err := s.db.GetSandboxByShortID(shortID)
	if err != nil || dbSbx == nil {
		return nil, false
	}
	return dbSandboxToSandbox(dbSbx), true
}

// Resolve finds a sandbox by either short ID or UUID.
// UUIDs are 36 chars; short IDs are 16 chars or fewer. We use 20 as the threshold.
func (s *Store) Resolve(idOrShortID string) (*Sandbox, bool) {
	if len(idOrShortID) <= 20 {
		if sbx, ok := s.GetByShortID(idOrShortID); ok {
			return sbx, true
		}
		return s.Get(idOrShortID)
	}
	if sbx, ok := s.Get(idOrShortID); ok {
		return sbx, true
	}
	return s.GetByShortID(idOrShortID)
}

// ListByWorkspace returns all sandboxes for a workspace from the database.
func (s *Store) ListByWorkspace(workspaceID string) []*Sandbox {
	dbSandboxes, err := s.db.ListSandboxesByWorkspace(workspaceID)
	if err != nil {
		return nil
	}
	out := make([]*Sandbox, 0, len(dbSandboxes))
	for _, ds := range dbSandboxes {
		out = append(out, dbSandboxToSandbox(ds))
	}
	return out
}

// UpdateStatus transitions a sandbox to a new status.
func (s *Store) UpdateStatus(id, status string) error {
	return s.db.UpdateSandboxStatus(id, status)
}

// Delete removes a sandbox from the DB.
func (s *Store) Delete(id string) error {
	return s.db.DeleteSandbox(id)
}

// UpdateActivity records user activity on a sandbox.
func (s *Store) UpdateActivity(id string) {
	s.db.UpdateSandboxActivity(id)
}

func dbSandboxToSandbox(ds *db.Sandbox) *Sandbox {
	sbx := &Sandbox{
		ID:          ds.ID,
		WorkspaceID: ds.WorkspaceID,
		Name:        ds.Name,
		Type:        ds.Type,
		Status:      ds.Status,
		CreatedAt:   ds.CreatedAt,
		IsLocal:     ds.IsLocal,
	}
	if ds.ShortID.Valid {
		sbx.ShortID = ds.ShortID.String
	}
	if ds.SandboxName.Valid {
		sbx.SandboxName = ds.SandboxName.String
	}
	if ds.PodIP.Valid {
		sbx.PodIP = ds.PodIP.String
	}
	if ds.OpencodeToken.Valid {
		sbx.OpencodeToken = ds.OpencodeToken.String
	}
	if ds.ProxyToken.Valid {
		sbx.ProxyToken = ds.ProxyToken.String
	}
	if ds.OpenclawToken.Valid {
		sbx.OpenclawToken = ds.OpenclawToken.String
	}
	if ds.LastActivityAt.Valid {
		t := ds.LastActivityAt.Time
		sbx.LastActivityAt = &t
	}
	if ds.PausedAt.Valid {
		t := ds.PausedAt.Time
		sbx.PausedAt = &t
	}
	if ds.TunnelToken.Valid {
		sbx.TunnelToken = ds.TunnelToken.String
	}
	if ds.LastHeartbeatAt.Valid {
		t := ds.LastHeartbeatAt.Time
		sbx.LastHeartbeatAt = &t
	}
	if ds.CPU != nil {
		sbx.CPU = *ds.CPU
	}
	if ds.Memory != nil {
		sbx.Memory = *ds.Memory
	}
	sbx.IdleTimeout = ds.IdleTimeout
	return sbx
}
