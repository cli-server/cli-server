package codexauth

import (
	"context"
	"os"
	"testing"

	"github.com/agentserver/agentserver/internal/db"
)

func newTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	d, err := db.Open(url)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	// Wipe codex_* tables for a clean slate.
	d.Exec(`DELETE FROM codex_agent_tasks`)
	d.Exec(`DELETE FROM codex_agent_identities`)
	d.Exec(`DELETE FROM codex_jwks_keys`)
	d.Exec(`DELETE FROM codex_device_codes`)
	d.Exec(`DELETE FROM codex_refresh_tokens`)
	d.Exec(`DELETE FROM codex_access_tokens`)
	d.Exec(`DELETE FROM codex_pkce_requests`)
	return NewStore(d), func() { d.Close() }
}

func TestStore_InsertJwksKey_RoundTrip(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	kid, kp, err := GenerateRSAKey()
	if err != nil {
		t.Fatalf("GenerateRSAKey: %v", err)
	}
	if err := s.InsertJwksKey(ctx, kid, kp, true); err != nil {
		t.Fatalf("InsertJwksKey: %v", err)
	}

	got, err := s.GetActiveJwksKey(ctx)
	if err != nil || got == nil {
		t.Fatalf("GetActiveJwksKey: %v %+v", err, got)
	}
	if got.Kid != kid {
		t.Errorf("kid = %q, want %q", got.Kid, kid)
	}
}

func TestStore_OnlyOneActiveKey(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	kid1, kp1, _ := GenerateRSAKey()
	kid2, kp2, _ := GenerateRSAKey()
	if err := s.InsertJwksKey(ctx, kid1, kp1, true); err != nil {
		t.Fatalf("InsertJwksKey #1: %v", err)
	}
	err := s.InsertJwksKey(ctx, kid2, kp2, true)
	if err == nil {
		t.Fatal("InsertJwksKey #2 should fail uniq_codex_jwks_keys_one_active")
	}
}

func TestStore_ListAllJwksKeys(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	kid1, kp1, _ := GenerateRSAKey()
	kid2, kp2, _ := GenerateRSAKey()
	s.InsertJwksKey(ctx, kid1, kp1, true)
	s.InsertJwksKey(ctx, kid2, kp2, false)

	keys, err := s.ListAllJwksKeys(ctx)
	if err != nil {
		t.Fatalf("ListAllJwksKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("keys count = %d, want 2", len(keys))
	}
}
