package codexauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const pkceCodeTTL = 10 * time.Minute

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
		next := url.QueryEscape(r.URL.RequestURI())
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
