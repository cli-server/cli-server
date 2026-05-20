package codexauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
