package codexauth

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	deviceCodeTTL      = 15 * time.Minute
	devicePollInterval = 5
)

// userCodeAlphabet excludes I, O, 0, 1 to reduce read-aloud confusion.
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func (s *Server) handleDeviceUserCode(w http.ResponseWriter, r *http.Request) {
	deviceAuthID := mustRandomHex(32)
	userCode := mustRandomUserCode()
	verifier := mustRandomHex(32) // 64 chars hex, fits 43-128 URL-safe
	challenge := base64URLNoPad(sha256Sum([]byte(verifier)))
	authorizationCode := mustRandomHex(32)

	if err := s.Store.InsertDeviceCode(r.Context(), DeviceCode{
		DeviceAuthID:      deviceAuthID,
		UserCode:          userCode,
		CodeChallenge:     challenge,
		CodeVerifier:      verifier,
		AuthorizationCode: authorizationCode,
		Status:            "pending",
		ExpiresAt:         time.Now().Add(deviceCodeTTL),
	}); err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_auth_id": deviceAuthID,
		"user_code":      userCode,
		"interval":       devicePollInterval,
	})
}

func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if req.DeviceAuthID == "" || req.UserCode == "" {
		http.Error(w, "device_auth_id and user_code required", http.StatusBadRequest)
		return
	}
	row, err := s.Store.GetDeviceCodeByUserCode(r.Context(), req.UserCode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if row == nil || row.DeviceAuthID != req.DeviceAuthID {
		// 404 keeps codex polling until expiry.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch row.Status {
	case "pending":
		// 403 keeps codex polling (device_code_auth.rs:128-138).
		http.Error(w, "authorization pending", http.StatusForbidden)
		return
	case "denied", "exchanged":
		http.Error(w, "not found", http.StatusNotFound)
		return
	case "approved":
		// fall through
	}

	dc, err := s.Store.ExchangeDeviceCode(r.Context(), req.DeviceAuthID, req.UserCode)
	if err != nil || dc == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Insert a matching codex_pkce_requests row so the subsequent
	// /oauth/token call (grant_type=authorization_code) succeeds.
	if err := s.Store.InsertPkceRequest(r.Context(), PkceRequest{
		Code:          dc.AuthorizationCode,
		CodeChallenge: dc.CodeChallenge,
		State:         "device-flow",
		UserID:        dc.UserID,
		ExpiresAt:     time.Now().Add(pkceCodeTTL),
	}); err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"authorization_code": dc.AuthorizationCode,
		"code_challenge":     dc.CodeChallenge,
		"code_verifier":      dc.CodeVerifier,
	})
}

const verifyFormHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Codex device login</title>
<style>
  body { font-family: -apple-system, sans-serif; max-width: 480px; margin: 4em auto; padding: 0 1em; }
  input[name=user_code] { font-family: monospace; font-size: 1.4em; letter-spacing: 0.1em;
                          text-transform: uppercase; padding: 0.5em; width: 100%%; }
  button { padding: 0.6em 1.2em; margin-right: 1em; font-size: 1em; }
  .deny { background: #fee; }
</style></head><body>
<h1>Codex device login</h1>
<p>Signed in as <code>%s</code>.</p>
<form method="POST" action="/codex/device">
  <p><label>Enter the code shown by <code>codex login --device-auth</code>:</label></p>
  <p><input name="user_code" autocomplete="off" autofocus required pattern="[A-Z0-9]{4}-[A-Z0-9]{4}"></p>
  <p><button name="action" value="approve">Approve</button>
     <button name="action" value="deny" class="deny">Deny</button></p>
</form>
</body></html>`

const verifyResultHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Codex device login</title>
<style>body{font-family:-apple-system,sans-serif;max-width:480px;margin:4em auto;padding:0 1em;}</style>
</head><body><h1>%s</h1><p>You may now return to your terminal.</p></body></html>`

func (s *Server) handleDeviceVerifyPage(w http.ResponseWriter, r *http.Request) {
	if s.SessionResolve == nil {
		http.Error(w, "session resolver not configured", http.StatusInternalServerError)
		return
	}
	uid := s.SessionResolve(r)
	if uid == "" {
		next := url.QueryEscape(absoluteRequestURL(r))
		http.Redirect(w, r, s.LoginRedirectURL+"?next="+next, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, verifyFormHTML, htmlEscape(s.lookupEmail(r.Context(), uid)))
}

func (s *Server) handleDeviceVerifySubmit(w http.ResponseWriter, r *http.Request) {
	if s.SessionResolve == nil {
		http.Error(w, "session resolver not configured", http.StatusInternalServerError)
		return
	}
	uid := s.SessionResolve(r)
	if uid == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form", http.StatusBadRequest)
		return
	}
	userCode := strings.ToUpper(strings.TrimSpace(r.PostForm.Get("user_code")))
	action := r.PostForm.Get("action")

	switch action {
	case "approve":
		if err := s.Store.ApproveDeviceCode(r.Context(), userCode, uid); err != nil {
			http.Error(w, "code not found or already used: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, verifyResultHTML, "Approved ✓")
	case "deny":
		_, _ = s.Store.db.ExecContext(r.Context(),
			`UPDATE codex_device_codes SET status = 'denied'
			 WHERE user_code = $1 AND status = 'pending'`, userCode)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, verifyResultHTML, "Denied ✗")
	default:
		http.Error(w, "action must be approve|deny", http.StatusBadRequest)
	}
}

func htmlEscape(s string) string {
	// Minimal — only the email displayed in the form.
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func mustRandomUserCode() string {
	// 8 chars in "XXXX-XXXX" form.
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("rand: " + err.Error())
	}
	pick := func(i byte) byte { return userCodeAlphabet[int(i)%len(userCodeAlphabet)] }
	return fmt.Sprintf("%c%c%c%c-%c%c%c%c",
		pick(b[0]), pick(b[1]), pick(b[2]), pick(b[3]),
		pick(b[4]), pick(b[5]), pick(b[6]), pick(b[7]))
}
