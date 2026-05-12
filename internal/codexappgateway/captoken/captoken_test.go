package captoken_test

import (
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/captoken"
	codexexecgateway "github.com/agentserver/agentserver/internal/codexexecgateway"
)

var testSecret = []byte("supersecret-test-key-for-captoken")

// TestMint_VerifyRoundTrip is the cross-service contract test. It mints a
// token with this package and verifies it with
// codexexecgateway.VerifyCapabilityToken. If this test fails, the two
// sides of the format have drifted apart.
func TestMint_VerifyRoundTrip(t *testing.T) {
	now := time.Now()
	exp := now.Add(24 * time.Hour).Unix()
	p := captoken.Payload{
		TurnID:      "thr_abc",
		WorkspaceID: "ws_xyz",
		ExeIDs:      []string{"exe_1"},
		IAT:         now.Unix(),
		EXP:         exp,
	}
	tok := captoken.Mint(testSecret, p)
	if tok == "" {
		t.Fatal("Mint returned empty token")
	}

	got, err := codexexecgateway.VerifyCapabilityToken(tok, testSecret)
	if err != nil {
		t.Fatalf("VerifyCapabilityToken: %v", err)
	}
	if got.TurnID != p.TurnID {
		t.Errorf("TurnID: got %q, want %q", got.TurnID, p.TurnID)
	}
	if got.WorkspaceID != p.WorkspaceID {
		t.Errorf("WorkspaceID: got %q, want %q", got.WorkspaceID, p.WorkspaceID)
	}
	if len(got.ExeIDs) != 1 || got.ExeIDs[0] != p.ExeIDs[0] {
		t.Errorf("ExeIDs: got %v, want %v", got.ExeIDs, p.ExeIDs)
	}
	if got.IAT != p.IAT {
		t.Errorf("IAT: got %d, want %d", got.IAT, p.IAT)
	}
	if got.EXP != p.EXP {
		t.Errorf("EXP: got %d, want %d", got.EXP, p.EXP)
	}
}

// TestMint_DifferentSecretsProduceDifferentSigs asserts that tokens minted
// with different secrets are distinct (different HMAC signatures).
func TestMint_DifferentSecretsProduceDifferentSigs(t *testing.T) {
	p := captoken.Payload{
		TurnID:      "thr_1",
		WorkspaceID: "ws_1",
		ExeIDs:      []string{"exe_1"},
		IAT:         1000,
		EXP:         9999999999,
	}
	tok1 := captoken.Mint([]byte("secret-a"), p)
	tok2 := captoken.Mint([]byte("secret-b"), p)
	if tok1 == tok2 {
		t.Error("tokens minted with different secrets are equal; expected distinct signatures")
	}

	// tok1 should fail verification under secret-b.
	_, err := codexexecgateway.VerifyCapabilityToken(tok1, []byte("secret-b"))
	if err == nil {
		t.Error("expected verification failure when using wrong secret, but got nil error")
	}
}

// TestMint_PayloadSurvivesRoundTrip exercises non-trivial payload values:
// multiple exe_ids, large EXP, non-ASCII-safe IDs.
func TestMint_PayloadSurvivesRoundTrip(t *testing.T) {
	p := captoken.Payload{
		TurnID:      "thr_with-dashes_and.dots",
		WorkspaceID: "ws_long-workspace-identifier-12345",
		ExeIDs:      []string{"exe_alpha", "exe_beta", "exe_gamma"},
		IAT:         1_700_000_000,
		EXP:         9_999_999_999,
	}
	tok := captoken.Mint(testSecret, p)

	got, err := codexexecgateway.VerifyCapabilityToken(tok, testSecret)
	if err != nil {
		t.Fatalf("VerifyCapabilityToken: %v", err)
	}
	if got.TurnID != p.TurnID {
		t.Errorf("TurnID: got %q, want %q", got.TurnID, p.TurnID)
	}
	if got.WorkspaceID != p.WorkspaceID {
		t.Errorf("WorkspaceID: got %q, want %q", got.WorkspaceID, p.WorkspaceID)
	}
	if len(got.ExeIDs) != len(p.ExeIDs) {
		t.Fatalf("ExeIDs len: got %d, want %d", len(got.ExeIDs), len(p.ExeIDs))
	}
	for i := range p.ExeIDs {
		if got.ExeIDs[i] != p.ExeIDs[i] {
			t.Errorf("ExeIDs[%d]: got %q, want %q", i, got.ExeIDs[i], p.ExeIDs[i])
		}
	}
	if got.IAT != p.IAT {
		t.Errorf("IAT: got %d, want %d", got.IAT, p.IAT)
	}
	if got.EXP != p.EXP {
		t.Errorf("EXP: got %d, want %d", got.EXP, p.EXP)
	}
}
