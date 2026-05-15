package server

import (
	"os"
	"testing"

	"github.com/agentserver/agentserver/internal/db"
)

func newCodexTestDBForServer(t *testing.T) *db.DB {
	t.Helper()
	d, err := openTestDBForServer(t)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM codex_remote_tokens`)
	})
	return d
}

func openTestDBForServer(t *testing.T) (*db.DB, error) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	d, err := db.Open(url)
	if err == nil {
		t.Cleanup(func() { d.Close() })
	}
	return d, err
}

func seedWorkspaceMember(t *testing.T, d *db.DB, wid, uid, role string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO workspaces (id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, wid, "test ws"); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO users (id, email) VALUES ($1, $2) ON CONFLICT DO NOTHING`, uid, uid+"@test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, wid, uid, role); err != nil {
		t.Fatalf("insert member: %v", err)
	}
}
