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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Identity is what a verified token decodes to.
type Identity struct {
	WorkspaceID string
	ThreadID    string
}

// Authenticator is the seam for inbound auth. Phase-1 impl is HMAC.
type Authenticator interface {
	Verify(token string) (Identity, error)
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
// The sig is always the last dot-separated field. The workspace_id is
// always the first dot-separated field in the prefix (everything before
// the sig); the thread_id is everything between the first and last dots.
// This means thread_id may contain dots but workspace_id may not.
//
// Last dot separates the sig from the (workspace_id, thread_id, ...) prefix.
// We split into "head" and "sig" instead of 3 fixed parts so thread
// ids that themselves contain dots verify correctly.
func (a *HMAC) Verify(token string) (Identity, error) {
	lastDot := strings.LastIndex(token, ".")
	if lastDot < 0 || lastDot == 0 || lastDot == len(token)-1 {
		return Identity{}, errors.New("auth: malformed token")
	}
	head, sig := token[:lastDot], token[lastDot+1:]
	sep := strings.IndexByte(head, '.')
	if sep < 0 || sep == 0 || sep == len(head)-1 {
		return Identity{}, errors.New("auth: malformed token")
	}
	workspaceID, threadID := head[:sep], head[sep+1:]
	expected := a.Mint(workspaceID, threadID)
	if !hmac.Equal([]byte(expected), []byte(workspaceID+"."+threadID+"."+sig)) {
		return Identity{}, errors.New("auth: signature mismatch")
	}
	return Identity{WorkspaceID: workspaceID, ThreadID: threadID}, nil
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
