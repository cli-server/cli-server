package server

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/agentserver/agentserver/internal/db"
)

// testDatabaseURL returns the TEST_DATABASE_URL environment variable, or empty
// string if not set. Integration tests call t.Skip when empty.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	return os.Getenv("TEST_DATABASE_URL")
}

// openTestDB opens a database connection via db.Open, which also runs migrations.
func openTestDB(url string) (*db.DB, error) {
	return db.Open(url)
}

var wsSeq atomic.Int64

// testWorkspaceID creates a throwaway workspace row and returns its ID.
// Each call produces a unique workspace to isolate tests.
func testWorkspaceID(t *testing.T, s *Server) string {
	t.Helper()
	n := wsSeq.Add(1)
	id := fmt.Sprintf("test-ws-%d", n)
	_, err := s.DB.Exec(
		`INSERT INTO workspaces (id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		id, fmt.Sprintf("Test Workspace %d", n),
	)
	if err != nil {
		t.Fatalf("create test workspace: %v", err)
	}
	t.Cleanup(func() {
		// CASCADE deletes credential_bindings rows.
		s.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, id)
	})
	return id
}
