package notebookjwt

import (
	"strings"
	"testing"
	"time"
)

func TestMintVerifyRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	tok, err := Mint(secret, "u-1", "ws-1", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	c, err := Verify(secret, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.UserID != "u-1" || c.WorkspaceID != "ws-1" {
		t.Errorf("claims=%+v", c)
	}
	if c.Exp < time.Now().Unix() {
		t.Errorf("exp in past: %d", c.Exp)
	}
}

func TestVerify_ExpiredRejected(t *testing.T) {
	secret := []byte("s")
	tok, err := Mint(secret, "u", "w", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(secret, tok); err == nil {
		t.Fatal("expected expired error")
	}
}

func TestVerify_TamperedRejected(t *testing.T) {
	secret := []byte("s")
	tok, _ := Mint(secret, "u", "w", time.Minute)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("parts=%d", len(parts))
	}
	bad := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	if _, err := Verify(secret, bad); err == nil {
		t.Fatal("expected tampered error")
	}
}

func TestVerify_WrongSecretRejected(t *testing.T) {
	tok, _ := Mint([]byte("right"), "u", "w", time.Minute)
	if _, err := Verify([]byte("wrong"), tok); err == nil {
		t.Fatal("expected wrong-secret error")
	}
}

func TestMint_EmptyArgs(t *testing.T) {
	if _, err := Mint(nil, "u", "w", time.Minute); err == nil {
		t.Error("nil secret should error")
	}
	if _, err := Mint([]byte("s"), "", "w", time.Minute); err == nil {
		t.Error("empty user_id should error")
	}
	if _, err := Mint([]byte("s"), "u", "", time.Minute); err == nil {
		t.Error("empty workspace_id should error")
	}
}
