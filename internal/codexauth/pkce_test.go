package codexauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/db"
	"github.com/go-chi/chi/v5"
)

func TestAuthorize_RedirectsUnauthToLogin(t *testing.T) {
	srv := newAuthTestServer(t, "")
	r := chi.NewRouter()
	srv.Mount(r)

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?state=xyz&code_challenge=abc&redirect_uri=http://localhost:1455/auth/callback", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	loc, _ := url.Parse(rr.Header().Get("Location"))
	if loc.Path != "/login" {
		t.Errorf("redirect path = %q, want /login", loc.Path)
	}
	if loc.Query().Get("next") == "" {
		t.Error("missing next param on login redirect")
	}
}

func TestAuthorize_AuthedRedirectsToRedirectURIWithCode(t *testing.T) {
	srv := newAuthTestServer(t, "user-abc")
	r := chi.NewRouter()
	srv.Mount(r)

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?state=st-1&code_challenge=ch-1&redirect_uri=http://localhost:1455/auth/callback", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	loc, _ := url.Parse(rr.Header().Get("Location"))
	if loc.Host != "localhost:1455" || loc.Path != "/auth/callback" {
		t.Errorf("redirect = %s", loc.String())
	}
	q := loc.Query()
	if q.Get("state") != "st-1" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("code") == "" {
		t.Error("missing code")
	}
}

// newAuthTestServer builds a Server backed by the real test DB (skips
// when TEST_DATABASE_URL unset) with a fixed session user.
func newAuthTestServer(t *testing.T, sessionUserID string) *Server {
	t.Helper()
	store, cleanup := newTestStore(t)
	t.Cleanup(cleanup)
	kid, kp, _ := GenerateRSAKey()
	ctx := context.Background()
	store.InsertJwksKey(ctx, kid, kp, true)
	// Pre-create the user if the test wants an authed session.
	if sessionUserID != "" {
		mustCreateTestUserWithID(t, store.db, sessionUserID)
	}
	return &Server{
		Store:            store,
		IssuerURL:        "https://test/codex-auth",
		SigningKey:       kp.PrivateKey,
		SigningKid:       kid,
		SessionResolve:   func(r *http.Request) string { return sessionUserID },
		LoginRedirectURL: "/login",
	}
}

// mustCreateTestUserWithID inserts a user with the exact id we want.
// Helper used by tests that need a specific session user id.
func mustCreateTestUserWithID(t *testing.T, d *db.DB, uid string) {
	t.Helper()
	_, err := d.Exec(`INSERT INTO users (id, username, email, created_at)
		VALUES ($1, $2, $3, NOW()) ON CONFLICT (id) DO NOTHING`,
		uid, "test-"+uid[:minInt(8, len(uid))], uid+"@test.local")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestToken_AuthorizationCode_ReturnsTriplet(t *testing.T) {
	srv := newAuthTestServer(t, "")
	ctx := context.Background()

	// Pre-seed an authorize row.
	uid := mustCreateTestUser(t, srv.Store.db)
	verifier := strings.Repeat("a", 48)
	challenge := base64URLNoPad(sha256Sum([]byte(verifier)))
	srv.Store.InsertPkceRequest(ctx, PkceRequest{
		Code:          "code-xyz",
		CodeChallenge: challenge,
		State:         "irrelevant",
		UserID:        uid,
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	})

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "code-xyz")
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", "http://localhost:1455/auth/callback")
	form.Set("client_id", "app_EMoamEEZ73f0CkXaXp7hrann")

	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	srv.Mount(r)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("status = %d, body = %s", rr.Code, body)
	}
	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.AccessToken == "" || resp.RefreshToken == "" || resp.IDToken == "" {
		t.Fatalf("missing fields: %+v", resp)
	}
	if resp.TokenType != "Bearer" {
		t.Errorf("token_type = %q", resp.TokenType)
	}
}

func TestToken_AuthorizationCode_BadVerifierRejected(t *testing.T) {
	srv := newAuthTestServer(t, "")
	ctx := context.Background()
	uid := mustCreateTestUser(t, srv.Store.db)
	srv.Store.InsertPkceRequest(ctx, PkceRequest{
		Code:          "code-bad",
		CodeChallenge: "different-challenge",
		State:         "x",
		UserID:        uid,
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	})
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "code-bad")
	form.Set("code_verifier", "wrong-verifier")
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	srv.Mount(r)
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestToken_Refresh_RotatesTokens(t *testing.T) {
	srv := newAuthTestServer(t, "")
	ctx := context.Background()
	uid := mustCreateTestUser(t, srv.Store.db)

	// Issue an initial refresh token directly.
	srv.Store.InsertRefreshToken(ctx, HashToken("old-refresh"), mustNewUUIDString(), uid,
		time.Now().Add(7*24*time.Hour))

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", "old-refresh")
	form.Set("client_id", "app_EMoamEEZ73f0CkXaXp7hrann")
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	srv.Mount(r)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.AccessToken == "" || resp.RefreshToken == "" || resp.RefreshToken == "old-refresh" {
		t.Errorf("bad rotated tokens: %+v", resp)
	}
}

func TestToken_Refresh_ExpiredReturnsTerminalError(t *testing.T) {
	srv := newAuthTestServer(t, "")
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", "never-issued")
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	srv.Mount(r)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	var resp map[string]string
	json.NewDecoder(rr.Body).Decode(&resp)
	// Codex treats these specific error codes as terminal (manager.rs:1050+).
	switch resp["error"] {
	case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		// OK
	default:
		t.Errorf("error = %q; expected one of refresh_token_{expired,reused,invalidated}",
			resp["error"])
	}
}

func TestToken_TokenExchange_ReturnsBadRequest(t *testing.T) {
	srv := newAuthTestServer(t, "")
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("requested_token", "openai-api-key")
	form.Set("subject_token", "irrelevant")
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:id_token")
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	srv.Mount(r)
	r.ServeHTTP(rr, req)
	// Codex tolerates failure (server.rs:352-354) — return 400 to opt out.
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}
