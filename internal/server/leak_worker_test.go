package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/bridge"
	"github.com/agentserver/agentserver/internal/db"
)

// newTestServerForLeak wires BridgeHandler + DB. Skips if TEST_DATABASE_URL absent.
func newTestServerForLeak(t *testing.T, ccBrokerURL string) (*Server, func()) {
	t.Helper()
	s, cleanup := newTestServerForEvents(t, ccBrokerURL)
	return s, cleanup
}

func TestLeakWorker_ClearsStaleActiveTurn(t *testing.T) {
	// cc-broker reports no active turn (turn_id: null).
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"turn_id":null}`))
	}))
	defer cc.Close()

	s, cleanup := newTestServerForLeak(t, cc.URL)
	defer cleanup()

	sid := "cse_leak_stale_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:              sid,
		WorkspaceID:     "ws_test",
		ExternalID:      "tui:exe_a:leak_stale",
		CreatorUserID:   "u_test",
		PermissionMode:  "ask",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	if _, err := s.DB.ClaimActiveTurn(context.Background(), sid, "trn_dead"); err != nil {
		t.Fatalf("ClaimActiveTurn: %v", err)
	}
	// Backdate updated_at so it appears stale.
	s.DB.Exec(`UPDATE agent_sessions SET updated_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, sid)

	lw := NewLeakWorker(s, LeakWorkerConfig{
		StaleTurnAfter: 5 * time.Minute,
		ResponderTTL:   90 * time.Second,
	})
	lw.RunOnce(context.Background())

	cur, err := s.DB.GetActiveTurn(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetActiveTurn: %v", err)
	}
	if cur != "" {
		t.Errorf("active_turn_id=%q want empty (should have been cleared)", cur)
	}
}

func TestLeakWorker_PreservesActiveTurnIfCCBrokerStillReports(t *testing.T) {
	activeTurnID := "trn_alive"
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// cc-broker still reports this turn as active.
		resp := map[string]any{"turn_id": activeTurnID}
		json.NewEncoder(w).Encode(resp)
	}))
	defer cc.Close()

	s, cleanup := newTestServerForLeak(t, cc.URL)
	defer cleanup()

	sid := "cse_leak_alive_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:              sid,
		WorkspaceID:     "ws_test",
		ExternalID:      "tui:exe_a:leak_alive",
		CreatorUserID:   "u_test",
		PermissionMode:  "ask",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	if _, err := s.DB.ClaimActiveTurn(context.Background(), sid, activeTurnID); err != nil {
		t.Fatalf("ClaimActiveTurn: %v", err)
	}
	s.DB.Exec(`UPDATE agent_sessions SET updated_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, sid)

	lw := NewLeakWorker(s, LeakWorkerConfig{
		StaleTurnAfter: 5 * time.Minute,
		ResponderTTL:   90 * time.Second,
	})
	lw.RunOnce(context.Background())

	cur, err := s.DB.GetActiveTurn(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetActiveTurn: %v", err)
	}
	if cur != activeTurnID {
		t.Errorf("active_turn_id=%q want %q (should NOT be cleared)", cur, activeTurnID)
	}
}

func TestLeakWorker_ClearsStaleResponder(t *testing.T) {
	s, cleanup := newTestServerForLeak(t, "")
	defer cleanup()
	s.BridgeHandler = &bridge.Handler{SSE: bridge.NewSSEBroker()}

	sid := "cse_leak_resp_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:              sid,
		WorkspaceID:     "ws_test",
		ExternalID:      "tui:exe_a:leak_resp",
		CreatorUserID:   "u_test",
		PermissionMode:  "ask",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	if _, err := s.DB.AttachResponder(context.Background(), sid, "exe_a", false); err != nil {
		t.Fatalf("AttachResponder: %v", err)
	}
	// Backdate responder_attached_at.
	s.DB.Exec(`UPDATE agent_sessions SET responder_attached_at = NOW() - INTERVAL '5 minutes' WHERE id = $1`, sid)

	lw := NewLeakWorker(s, LeakWorkerConfig{
		StaleTurnAfter: 5 * time.Minute,
		ResponderTTL:   90 * time.Second,
	})
	lw.RunOnce(context.Background())

	sess, err := s.DB.GetAgentSession(sid)
	if err != nil || sess == nil {
		t.Fatalf("GetAgentSession: %v", err)
	}
	if sess.PermissionResponder != nil {
		t.Errorf("permission_responder=%q want nil (should have been cleared by TTL)", *sess.PermissionResponder)
	}
}
