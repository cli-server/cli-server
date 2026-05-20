package codexauth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

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

func TestStore_PkceRequest_RoundTrip(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	err := s.InsertPkceRequest(ctx, PkceRequest{
		Code:          "code-abc",
		CodeChallenge: "chall-123",
		State:         "state-xyz",
		UserID:        uid,
		ExpiresAt:     time.Now().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("InsertPkceRequest: %v", err)
	}
	got, err := s.ConsumePkceRequest(ctx, "code-abc")
	if err != nil || got == nil {
		t.Fatalf("ConsumePkceRequest: %v %+v", err, got)
	}
	if got.CodeChallenge != "chall-123" {
		t.Errorf("CodeChallenge = %q", got.CodeChallenge)
	}
	// ConsumePkceRequest deletes — second call returns nil.
	got2, _ := s.ConsumePkceRequest(ctx, "code-abc")
	if got2 != nil {
		t.Error("second consume should return nil")
	}
}

func TestStore_AccessToken_HashLookup(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	hash := HashToken("raw-access-token")
	exp := time.Now().Add(1 * time.Hour).UTC()
	if err := s.InsertAccessToken(ctx, hash, uid, exp); err != nil {
		t.Fatalf("InsertAccessToken: %v", err)
	}
	gotUID, err := s.LookupAccessToken(ctx, "raw-access-token")
	if err != nil {
		t.Fatalf("LookupAccessToken: %v", err)
	}
	if gotUID != uid {
		t.Errorf("uid = %q, want %q", gotUID, uid)
	}
}

func TestStore_AccessToken_ExpiredReturnsEmpty(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	hash := HashToken("raw-expired-token")
	if err := s.InsertAccessToken(ctx, hash, uid, time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("InsertAccessToken: %v", err)
	}
	gotUID, _ := s.LookupAccessToken(ctx, "raw-expired-token")
	if gotUID != "" {
		t.Errorf("uid = %q, expected empty for expired token", gotUID)
	}
}

func TestStore_RefreshToken_RotateInvalidatesOld(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	family := mustNewUUID(t, s.db)
	oldHash := HashToken("refresh-old")
	if err := s.InsertRefreshToken(ctx, oldHash, family, uid, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("InsertRefreshToken: %v", err)
	}

	// Look up + revoke old, insert new, all in one call.
	newHash := HashToken("refresh-new")
	gotUID, err := s.RotateRefreshToken(ctx, "refresh-old", newHash, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}
	if gotUID != uid {
		t.Errorf("uid = %q, want %q", gotUID, uid)
	}
	// Old is now revoked.
	if _, err := s.RotateRefreshToken(ctx, "refresh-old", newHash, time.Now().Add(24*time.Hour)); err == nil {
		t.Error("rotating revoked token should error")
	}
}

func TestStore_RefreshToken_FamilyIDPreserved(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	family := mustNewUUID(t, s.db)
	s.InsertRefreshToken(ctx, HashToken("rt-1"), family, uid, time.Now().Add(24*time.Hour))
	s.RotateRefreshToken(ctx, "rt-1", HashToken("rt-2"), time.Now().Add(24*time.Hour))

	var got string
	if err := s.db.QueryRow(
		`SELECT family_id::text FROM codex_refresh_tokens WHERE token_hash = $1`,
		HashToken("rt-2"),
	).Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != family {
		t.Errorf("family_id = %q, want %q", got, family)
	}
}

func TestStore_RefreshToken_ReuseRevokesFamily(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	family := mustNewUUID(t, s.db)
	// Insert two siblings in the same family.
	s.InsertRefreshToken(ctx, HashToken("rt-a"), family, uid, time.Now().Add(24*time.Hour))
	s.InsertRefreshToken(ctx, HashToken("rt-b"), family, uid, time.Now().Add(24*time.Hour))

	// Rotate rt-a → rt-c (legitimate use, succeeds).
	if _, err := s.RotateRefreshToken(ctx, "rt-a", HashToken("rt-c"), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("first rotate: %v", err)
	}

	// Attempted reuse of revoked rt-a — must error with ErrRefreshTokenReuse
	// AND revoke the entire family (so rt-b and rt-c are now unusable).
	_, err := s.RotateRefreshToken(ctx, "rt-a", HashToken("rt-d"), time.Now().Add(24*time.Hour))
	if !errors.Is(err, ErrRefreshTokenReuse) {
		t.Errorf("err = %v, want ErrRefreshTokenReuse", err)
	}

	// rt-b should now be revoked too (family burned).
	_, err = s.RotateRefreshToken(ctx, "rt-b", HashToken("rt-e"), time.Now().Add(24*time.Hour))
	if !errors.Is(err, ErrRefreshTokenReuse) {
		t.Errorf("rt-b rotate after family burn: err = %v, want ErrRefreshTokenReuse", err)
	}
	// rt-c (the legitimate post-rotation token) should also be burned.
	_, err = s.RotateRefreshToken(ctx, "rt-c", HashToken("rt-f"), time.Now().Add(24*time.Hour))
	if !errors.Is(err, ErrRefreshTokenReuse) {
		t.Errorf("rt-c rotate after family burn: err = %v, want ErrRefreshTokenReuse", err)
	}
}

// --- helpers ---

func mustCreateTestUser(t *testing.T, d *db.DB) string {
	t.Helper()
	id := mustNewUUID(t, d)
	_, err := d.Exec(`INSERT INTO users (id, username, email, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (id) DO NOTHING`, id, "test-"+id[:8], id+"@test.local")
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	return id
}

func mustNewUUID(t *testing.T, d *db.DB) string {
	t.Helper()
	var s string
	if err := d.QueryRow("SELECT gen_random_uuid()::text").Scan(&s); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return s
}

func TestStore_AgentIdentity_RoundTrip(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	kid, kp, _ := GenerateRSAKey()
	s.InsertJwksKey(ctx, kid, kp, true)

	_, pub, _ := GenerateEd25519Key()
	rid := "exe_test_" + mustNewUUID(t, s.db)
	if err := s.InsertAgentIdentity(ctx, AgentIdentity{
		AgentRuntimeID: rid,
		UserID:         uid,
		PublicKey:      pub,
		JWTSignedWith:  kid,
		IssuedAt:       time.Now(),
		ExpiresAt:      time.Now().Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertAgentIdentity: %v", err)
	}

	got, err := s.GetAgentIdentity(ctx, rid)
	if err != nil || got == nil {
		t.Fatalf("GetAgentIdentity: %v %+v", err, got)
	}
	if got.UserID != uid || string(got.PublicKey) != string(pub) {
		t.Errorf("got = %+v", got)
	}
}

func TestStore_AgentTask_RoundTrip(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	kid, kp, _ := GenerateRSAKey()
	s.InsertJwksKey(ctx, kid, kp, true)
	_, pub, _ := GenerateEd25519Key()
	rid := "exe_task_" + mustNewUUID(t, s.db)
	s.InsertAgentIdentity(ctx, AgentIdentity{
		AgentRuntimeID: rid, UserID: uid, PublicKey: pub,
		JWTSignedWith: kid, IssuedAt: time.Now(),
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	})

	tid := "task_" + mustNewUUID(t, s.db)
	if err := s.InsertAgentTask(ctx, tid, rid, uid, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("InsertAgentTask: %v", err)
	}

	got, err := s.GetAgentTask(ctx, tid)
	if err != nil || got == nil {
		t.Fatalf("GetAgentTask: %v %+v", err, got)
	}
	if got.AgentRuntimeID != rid || got.UserID != uid {
		t.Errorf("got = %+v", got)
	}
}

func TestStore_DeviceCode_PendingToApproved(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	dc := DeviceCode{
		DeviceAuthID:      "dev-abc",
		UserCode:          "BDWD-HQPK",
		CodeChallenge:     "chall",
		CodeVerifier:      "ver",
		AuthorizationCode: "authcode-xyz",
		Status:            "pending",
		ExpiresAt:         time.Now().Add(15 * time.Minute),
	}
	if err := s.InsertDeviceCode(ctx, dc); err != nil {
		t.Fatalf("InsertDeviceCode: %v", err)
	}
	// Initially pending.
	got, _ := s.GetDeviceCodeByUserCode(ctx, "BDWD-HQPK")
	if got == nil || got.Status != "pending" {
		t.Fatalf("got = %+v", got)
	}
	// Approve.
	if err := s.ApproveDeviceCode(ctx, "BDWD-HQPK", uid); err != nil {
		t.Fatalf("ApproveDeviceCode: %v", err)
	}
	got2, _ := s.GetDeviceCodeByUserCode(ctx, "BDWD-HQPK")
	if got2.Status != "approved" || got2.UserID != uid {
		t.Errorf("after approve: %+v", got2)
	}
}

func TestStore_DeviceCode_ExchangeOnceOnly(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	uid := mustCreateTestUser(t, s.db)

	dc := DeviceCode{
		DeviceAuthID:      "dev-only-once",
		UserCode:          "ABCD-EFGH",
		CodeChallenge:     "chall",
		CodeVerifier:      "ver",
		AuthorizationCode: "authcode-xyz",
		Status:            "pending",
		ExpiresAt:         time.Now().Add(15 * time.Minute),
	}
	s.InsertDeviceCode(ctx, dc)
	s.ApproveDeviceCode(ctx, "ABCD-EFGH", uid)
	got, err := s.ExchangeDeviceCode(ctx, "dev-only-once", "ABCD-EFGH")
	if err != nil || got == nil {
		t.Fatalf("first exchange: %v %+v", err, got)
	}
	got2, _ := s.ExchangeDeviceCode(ctx, "dev-only-once", "ABCD-EFGH")
	if got2 != nil {
		t.Error("second exchange should return nil (single-use)")
	}
}
