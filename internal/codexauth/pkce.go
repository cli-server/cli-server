package codexauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	pkceCodeTTL     = 10 * time.Minute
	accessTokenTTL  = 1 * time.Hour
	refreshTokenTTL = 365 * 24 * time.Hour
)

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	challenge := q.Get("code_challenge")
	redirectURI := q.Get("redirect_uri")
	if state == "" || challenge == "" || redirectURI == "" {
		http.Error(w, "missing required oauth params (state, code_challenge, redirect_uri)",
			http.StatusBadRequest)
		return
	}

	userID := s.SessionResolve(r)
	if userID == "" {
		// next must be ABSOLUTE because the login redirect crosses
		// subdomains (codex-auth.<domain> → <domain> root). Browser
		// resolves the absolute URL correctly after SPA login.
		next := url.QueryEscape(absoluteRequestURL(r))
		http.Redirect(w, r, s.LoginRedirectURL+"?next="+next, http.StatusFound)
		return
	}

	code := mustRandomHex(32)
	if err := s.Store.InsertPkceRequest(r.Context(), PkceRequest{
		Code:          code,
		CodeChallenge: challenge,
		State:         state,
		UserID:        userID,
		ExpiresAt:     time.Now().Add(pkceCodeTTL),
	}); err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Redirect back to the codex local callback server with code + state.
	dest, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	dq := dest.Query()
	dq.Set("code", code)
	dq.Set("state", state)
	dest.RawQuery = dq.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

func mustRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeOauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

// pkceVerifierMatches implements RFC 7636 S256:
// base64url_no_pad(sha256(code_verifier)) == code_challenge.
func pkceVerifierMatches(verifier, challenge string) bool {
	sum := sha256Sum([]byte(verifier))
	expected := base64URLNoPad(sum)
	return expected == strings.TrimRight(challenge, "=")
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "parse form: "+err.Error())
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.handleTokenAuthorizationCode(w, r)
	case "refresh_token":
		s.handleTokenRefresh(w, r)
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		// We don't have an OpenAI sk-... to give back. Codex tolerates the
		// failure (login/src/server.rs:352-354).
		writeOauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"token-exchange to OpenAI API key is not supported on this issuer")
	default:
		writeOauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			r.PostForm.Get("grant_type"))
	}
}

func (s *Server) handleTokenAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	if code == "" || verifier == "" {
		writeOauthError(w, http.StatusBadRequest, "invalid_request",
			"code and code_verifier required")
		return
	}

	req, err := s.Store.ConsumePkceRequest(r.Context(), code)
	if err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if req == nil {
		writeOauthError(w, http.StatusBadRequest, "invalid_grant",
			"code unknown or expired")
		return
	}
	if !pkceVerifierMatches(verifier, req.CodeChallenge) {
		writeOauthError(w, http.StatusBadRequest, "invalid_grant",
			"code_verifier does not match code_challenge")
		return
	}

	s.mintTokensFor(w, r, req.UserID)
}

// mintTokensFor mints access + refresh + id_token and writes the codex-
// shaped response. Shared by authorization_code, refresh_token (B4), and
// the device-flow exchange path (B5).
//
// access_token is minted as a signed JWT with `{sub, exp}` so codex's
// proactive refresh logic (parse_jwt_expiration) works correctly.
// We still store its sha256 hash in codex_access_tokens for server-side
// revocation; the hash is what /internal/codex-auth/validate looks
// up (Phase D) — the JWT body is opaque to us.
func (s *Server) mintTokensFor(w http.ResponseWriter, r *http.Request, userID string) {
	ctx := r.Context()
	accessExp := time.Now().Add(accessTokenTTL)
	email := s.lookupEmail(ctx, userID)
	access, err := BuildAccessToken(s.SigningKey, s.SigningKid, IDTokenClaims{
		Issuer:    s.IssuerURL,
		Subject:   userID,
		Email:     email,
		ExpiresAt: accessExp,
	})
	if err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error",
			"build access_token: "+err.Error())
		return
	}
	refresh := mustRandomHex(32)

	if err := s.Store.InsertAccessToken(ctx, HashToken(access), userID, accessExp); err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	familyID := mustNewUUIDString()
	if err := s.Store.InsertRefreshToken(ctx, HashToken(refresh), familyID, userID,
		time.Now().Add(refreshTokenTTL)); err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	idTok, err := BuildIDToken(s.SigningKey, s.SigningKid, IDTokenClaims{
		Issuer:    s.IssuerURL,
		Subject:   userID,
		Email:     email,
		ExpiresAt: accessExp,
	})
	if err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error",
			"build id_token: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id_token":      idTok,
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
	})
}

func (s *Server) handleTokenRefresh(w http.ResponseWriter, r *http.Request) {
	raw := r.PostForm.Get("refresh_token")
	if raw == "" {
		writeOauthError(w, http.StatusBadRequest, "invalid_request",
			"refresh_token required")
		return
	}
	newRefresh := mustRandomHex(32)
	userID, err := s.Store.RotateRefreshToken(r.Context(), raw,
		HashToken(newRefresh), time.Now().Add(refreshTokenTTL))
	if err != nil {
		// Both "unknown token" and "already revoked (reuse)" surface as
		// ErrRefreshTokenReuse from the store; codex treats the
		// reuse/expired/invalidated codes as terminal (manager.rs:1050+).
		writeOauthError(w, http.StatusUnauthorized, "refresh_token_expired", err.Error())
		return
	}

	accessExp := time.Now().Add(accessTokenTTL)
	email := s.lookupEmail(r.Context(), userID)
	access, err := BuildAccessToken(s.SigningKey, s.SigningKid, IDTokenClaims{
		Issuer:    s.IssuerURL,
		Subject:   userID,
		Email:     email,
		ExpiresAt: accessExp,
	})
	if err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error",
			"build access_token: "+err.Error())
		return
	}
	if err := s.Store.InsertAccessToken(r.Context(), HashToken(access), userID, accessExp); err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	idTok, err := BuildIDToken(s.SigningKey, s.SigningKid, IDTokenClaims{
		Issuer:    s.IssuerURL,
		Subject:   userID,
		Email:     email,
		ExpiresAt: accessExp,
	})
	if err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id_token":      idTok,
		"access_token":  access,
		"refresh_token": newRefresh,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
	})
}

func (s *Server) lookupEmail(ctx context.Context, userID string) string {
	var email string
	_ = s.Store.db.QueryRowContext(ctx,
		`SELECT email FROM users WHERE id = $1`, userID).Scan(&email)
	if email == "" {
		email = userID + "@agent.cs.ac.cn"
	}
	return email
}

// mustNewUUIDString generates a UUIDv4 via crypto/rand without pulling
// in a new dep (matches the rest of the codebase, e.g. process/spawn.go).
func mustNewUUIDString() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("rand: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
