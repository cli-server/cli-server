package ccbroker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

func TestStoreImplementsTurnQueueOps(t *testing.T) {
	// Compile-time: *Store must satisfy the extended storer interface that
	// includes turn-queue ops. If this test file compiles, the contract holds.
	var _ storer = (*Store)(nil)
}

func TestMigrationApplies(t *testing.T) {
	url := getTestPostgresURL(t)
	if url == "" {
		t.Skip("no test postgres URL set; skipping")
	}
	st, err := NewStore(url)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer st.Close()

	sid := "sess_test_" + randomSuffix(t)
	wid := "ws_test"
	if err := st.CreateSession(context.Background(), sid, wid, "", "test", nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	turn := AgentTurn{
		ID: "trn_test_" + randomSuffix(t), SessionID: sid, WorkspaceID: wid,
		UserEventID: "evt_x", UserMessage: "hi",
	}
	if err := st.EnqueueTurn(context.Background(), turn); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	got, err := st.PickNextPending(context.Background(), sid)
	if err != nil {
		t.Fatalf("PickNextPending: %v", err)
	}
	if got == nil || got.ID != turn.ID || got.State != "queued" {
		t.Fatalf("unexpected pick: %+v", got)
	}
}

func getTestPostgresURL(t *testing.T) string {
	t.Helper()
	return os.Getenv("CCBROKER_TEST_POSTGRES_URL")
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}
