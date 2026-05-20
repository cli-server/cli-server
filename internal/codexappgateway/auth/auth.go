// Package auth handles inbound caller authentication for codex-app-gateway.
//
// Phase 1 uses an HMAC-signed token of the form
//
//	<workspace_id>.<thread_id>.<hex-hmac-sha256>
//
// where the HMAC covers `<workspace_id>\0<thread_id>` keyed by a
// deployment-shared secret. This matches the wstoken / internal-API
// pattern used elsewhere in agentserver and avoids pulling a JWT lib
// just for phase 1. Phase 2 can swap in a JWT impl behind the same
// `Authenticator` interface.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Identity is what a verified token decodes to.
type Identity struct {
	UserID      string
	WorkspaceID string
}

// Authenticator is the seam for inbound auth. Phase-1 impl is HMAC.
type Authenticator interface {
	Verify(ctx context.Context, token string) (Identity, error)
}

// SessionTracker is an optional capability for Authenticator implementations
// that also record per-connection sessions (RemoteVerifier does, HMAC does
// not). The handler in CXG type-asserts and prefers OpenSession when
// available so the Browsers panel can show live online state + client info.
type SessionTracker interface {
	OpenSession(ctx context.Context, token, clientIP, clientUA, codexVersion, osStr string) (Identity, string, error)
	CloseSession(ctx context.Context, sessionID string) error
}

// HMAC is the phase-1 Authenticator.
type HMAC struct{ secret []byte }

// NewHMAC returns a phase-1 Authenticator. The secret must be non-empty
// for tokens to verify; an empty secret will still mint and verify
// against itself but represents a deployment misconfiguration.
func NewHMAC(secret []byte) *HMAC { return &HMAC{secret: secret} }

// Mint produces a token for `(workspaceID, threadID)`. Useful for tests
// and CLI tools; production callers receive tokens from agentserver.
func (a *HMAC) Mint(workspaceID, threadID string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(workspaceID))
	mac.Write([]byte{0})
	mac.Write([]byte(threadID))
	return workspaceID + "." + threadID + "." + hex.EncodeToString(mac.Sum(nil))
}

// Verify parses and HMAC-verifies a token.
//
// Token format: <workspace_id>.<thread_id>.<hex-hmac>
//
// Expects exactly 3 dot-separated parts. The legacy threadID portion
// (parts[1]) is intentionally discarded — Phase 2 carries identity via
// the UserID field populated by RemoteVerifier, not by the token payload.
func (a *HMAC) Verify(_ context.Context, token string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Identity{}, errors.New("auth: malformed token")
	}
	expected := a.Mint(parts[0], parts[1])
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return Identity{}, errors.New("auth: signature mismatch")
	}
	// Phase-2: parts[1] (legacy threadID) is intentionally discarded.
	return Identity{WorkspaceID: parts[0]}, nil
}

// ExtractBearer pulls the token out of `Authorization: Bearer <tok>`.
func ExtractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimPrefix(h, prefix), true
}
