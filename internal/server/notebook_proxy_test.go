package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/notebookjwt"
	"github.com/agentserver/agentserver/internal/notebooksupervisor"
)

// newProxyTestServer constructs a minimal *Server wired with a test
// upstream hook and a non-nil supervisor. The supervisor instance is
// never actually called: testNotebookUpstream short-circuits the
// resolve path before EnsureRunning would fire.
func newProxyTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	s := &Server{
		NotebookSupervisor: &notebooksupervisor.Supervisor{},
		testNotebookUpstream: func(wsID string) (string, error) {
			return upstreamURL, nil
		},
	}
	return s
}

// proxyTestRouter mounts only the proxy route so tests don't pull in
// the rest of Server.Router(), which would require DB, auth, etc.
func proxyTestRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.HandleFunc("/api/notebooks/{ws}/*", s.notebookProxy)
	return r
}

func TestNotebookProxy_HTTPForwardsWithUserHeader(t *testing.T) {
	var gotUser, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-Forwarded-User")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	s := newProxyTestServer(t, upstream.URL)
	s.NotebookJWTSecret = []byte("s")

	tok, err := notebookjwt.Mint(s.NotebookJWTSecret, "u-1", "ws-1", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/lab?token="+url.QueryEscape(tok), nil)
	rr := httptest.NewRecorder()
	proxyTestRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "ok" {
		t.Errorf("body=%q", rr.Body.String())
	}
	if gotUser != "u-1" {
		t.Errorf("X-Forwarded-User=%q", gotUser)
	}
	if gotPath != "/lab" {
		t.Errorf("path=%q (should strip /api/notebooks/{ws})", gotPath)
	}
}

func TestNotebookProxy_MissingTokenRejected(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	s := newProxyTestServer(t, upstream.URL)
	s.NotebookJWTSecret = []byte("s")

	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/lab", nil)
	rr := httptest.NewRecorder()
	proxyTestRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

func TestNotebookProxy_BadTokenRejected(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	s := newProxyTestServer(t, upstream.URL)
	s.NotebookJWTSecret = []byte("s")

	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/lab?token=garbage", nil)
	rr := httptest.NewRecorder()
	proxyTestRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

func TestNotebookProxy_WrongWorkspaceRejected(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	s := newProxyTestServer(t, upstream.URL)
	s.NotebookJWTSecret = []byte("s")

	tok, err := notebookjwt.Mint(s.NotebookJWTSecret, "u-1", "OTHER-ws", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/lab?token="+url.QueryEscape(tok), nil)
	rr := httptest.NewRecorder()
	proxyTestRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status=%d, want 403 (JWT workspace mismatches URL)", rr.Code)
	}
}

func TestNotebookProxy_AuthorizationHeaderAccepted(t *testing.T) {
	var gotUser string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-Forwarded-User")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	s := newProxyTestServer(t, upstream.URL)
	s.NotebookJWTSecret = []byte("s")

	tok, err := notebookjwt.Mint(s.NotebookJWTSecret, "u-bearer", "ws-1", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/notebooks/ws-1/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	proxyTestRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotUser != "u-bearer" {
		t.Errorf("X-Forwarded-User=%q", gotUser)
	}
}
