package codexauth

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	agentIdentityJWTTTL   = 30 * 24 * time.Hour
	agentTaskTTL          = 24 * time.Hour
	taskRegisterClockSkew = 5 * time.Minute
)

// MintAgentIdentityArgs is the input shape used by the UI's "Add
// connector" path and the test bench.
type MintAgentIdentityArgs struct {
	AgentRuntimeID string // == exe_id
	UserID         string
	Email          string
}

// MintAgentIdentityResult is what the UI shows the user (JWT) and what
// tests use (privKey) to assert downstream signature checks pass.
type MintAgentIdentityResult struct {
	JWT     string
	privKey ed25519.PrivateKey // not exported; tests in same package read it
}

// MintAgentIdentity generates an Ed25519 keypair for the new agent,
// signs the Agent Identity JWT with the active RSA key, and persists
// (public key, jwt_signed_with) so subsequent AgentAssertion signatures
// can be verified.
func (s *Server) MintAgentIdentity(ctx context.Context, args MintAgentIdentityArgs) (*MintAgentIdentityResult, error) {
	if args.AgentRuntimeID == "" || args.UserID == "" {
		return nil, fmt.Errorf("AgentRuntimeID and UserID required")
	}

	pkcs8, pub, err := GenerateEd25519Key()
	if err != nil {
		return nil, fmt.Errorf("ed25519 generate: %w", err)
	}

	expires := time.Now().Add(agentIdentityJWTTTL)
	jwt, err := BuildAgentIdentityJWT(s.SigningKey, s.SigningKid, AgentIdentityClaims{
		AgentRuntimeID:       args.AgentRuntimeID,
		AgentPrivateKeyPKCS8: pkcs8,
		AccountID:            args.UserID,
		ChatgptUserID:        args.UserID,
		Email:                args.Email,
		PlanType:             "pro",
		ExpiresAt:            expires,
	})
	if err != nil {
		return nil, fmt.Errorf("build jwt: %w", err)
	}

	if err := s.Store.InsertAgentIdentity(ctx, AgentIdentity{
		AgentRuntimeID: args.AgentRuntimeID,
		UserID:         args.UserID,
		PublicKey:      pub,
		JWTSignedWith:  s.SigningKid,
		IssuedAt:       time.Now(),
		ExpiresAt:      expires,
	}); err != nil {
		return nil, err
	}

	// Recover the ed25519 private key for in-process tests / debug.
	parsed, _ := x509.ParsePKCS8PrivateKey(pkcs8)
	priv, _ := parsed.(ed25519.PrivateKey)
	return &MintAgentIdentityResult{JWT: jwt, privKey: priv}, nil
}

// handleTaskRegister handles POST /v1/agent/{rid}/task/register —
// called once per JWT load by codex. We verify the Ed25519 signature,
// issue a task_id, and persist it.
func (s *Server) handleTaskRegister(w http.ResponseWriter, r *http.Request) {
	rid := chi.URLParam(r, "rid")
	if rid == "" {
		http.Error(w, "rid required", http.StatusBadRequest)
		return
	}

	var req struct {
		Timestamp string `json:"timestamp"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	ts, err := time.Parse(time.RFC3339, req.Timestamp)
	if err != nil {
		http.Error(w, "bad timestamp", http.StatusBadRequest)
		return
	}
	if d := time.Since(ts); d > taskRegisterClockSkew || d < -taskRegisterClockSkew {
		http.Error(w, "timestamp stale", http.StatusUnauthorized)
		return
	}

	identity, err := s.Store.GetAgentIdentity(r.Context(), rid)
	if err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if identity == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}

	sig, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		http.Error(w, "bad signature base64", http.StatusBadRequest)
		return
	}
	message := []byte(rid + ":" + req.Timestamp)
	if !ed25519.Verify(ed25519.PublicKey(identity.PublicKey), message, sig) {
		http.Error(w, "signature invalid", http.StatusUnauthorized)
		return
	}

	taskID := "task_" + mustRandomHex(24)
	if err := s.Store.InsertAgentTask(r.Context(), taskID, rid, identity.UserID,
		time.Now().Add(agentTaskTTL)); err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"task_id": taskID})
}
