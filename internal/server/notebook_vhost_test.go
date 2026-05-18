package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/notebookjwt"
	"github.com/agentserver/agentserver/internal/notebooksupervisor"
)

// newVhostTestServer wires a Server with vhost enabled and a stubbed
// upstream so resolveNotebookUpstream doesn't touch k8s.
func newVhostTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	return &Server{
		NotebookSupervisor:      &notebooksupervisor.Supervisor{},
		NotebookJWTSecret:       []byte("test-secret-key"),
		NotebookHostBaseDomain:  "agent.test",
		NotebookSubdomainPrefix: "nb",
		testNotebookUpstream: func(wsID string) (string, error) {
			return upstreamURL, nil
		},
	}
}

func TestNotebookVhost_HostMatchExtractShort(t *testing.T) {
	s := &Server{NotebookHostBaseDomain: "agent.test", NotebookSubdomainPrefix: "nb"}
	cases := map[string]string{
		"nb-deadbeef.agent.test":      "deadbeef",
		"nb-deadbeef.agent.test:8443": "deadbeef",
		"nb-77f66719.agent.test":      "77f66719",
		"foo.agent.test":              "",
		"agent.test":                  "",
		"nb-x.other.test":             "",
		"other.com":                   "",
	}
	for host, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = host
		if got := s.notebookVhostHostMatch(r); got != want {
			t.Errorf("host=%q got=%q want=%q", host, got, want)
		}
	}
}

func TestNotebookVhost_AuthExchangeSetsCookieAndRedirects(t *testing.T) {
	s := newVhostTestServer(t, "http://upstream.invalid")
	wsID := "deadbeef-aaaa-bbbb-cccc-000000000001"
	tok, err := notebookjwt.Mint(s.NotebookJWTSecret, "u-1", wsID, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth?token="+url.QueryEscape(tok), nil)
	req.Host = "nb-deadbeef.agent.test"
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if loc != "/lab" {
		t.Errorf("Location=%q want /lab", loc)
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == notebookCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no nb-token cookie set")
	}
	if cookie.Value != tok {
		t.Errorf("cookie value mismatch")
	}
	if !cookie.HttpOnly || !cookie.Secure {
		t.Errorf("cookie flags HttpOnly=%v Secure=%v", cookie.HttpOnly, cookie.Secure)
	}
}

func TestNotebookVhost_AuthRejectsHostShortMismatch(t *testing.T) {
	s := newVhostTestServer(t, "http://upstream.invalid")
	wsID := "deadbeef-aaaa-bbbb-cccc-000000000001"
	tok, err := notebookjwt.Mint(s.NotebookJWTSecret, "u-1", wsID, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth?token="+url.QueryEscape(tok), nil)
	req.Host = "nb-cafebabe.agent.test" // wrong short
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rr.Code)
	}
}

func TestNotebookVhost_MissingCookieRejected(t *testing.T) {
	s := newVhostTestServer(t, "http://upstream.invalid")

	req := httptest.NewRequest(http.MethodGet, "/lab", nil)
	req.Host = "nb-deadbeef.agent.test"
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", rr.Code)
	}
}

func TestNotebookVhost_CookieValidThenProxies(t *testing.T) {
	var gotPath string
	var gotUser string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser = r.Header.Get("X-Forwarded-User")
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	s := newVhostTestServer(t, upstream.URL)
	wsID := "deadbeef-aaaa-bbbb-cccc-000000000001"
	tok, err := notebookjwt.Mint(s.NotebookJWTSecret, "u-vhost", wsID, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/lab/tree/foo.ipynb", nil)
	req.Host = "nb-deadbeef.agent.test"
	req.AddCookie(&http.Cookie{Name: notebookCookieName, Value: tok})
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotPath != "/lab/tree/foo.ipynb" {
		t.Errorf("upstream path=%q (should preserve full path, no strip)", gotPath)
	}
	if gotUser != "u-vhost" {
		t.Errorf("X-Forwarded-User=%q", gotUser)
	}
}

func TestNotebookVhost_DisabledBaseDomainDoesNotIntercept(t *testing.T) {
	// With NotebookHostBaseDomain="" the middleware shouldn't be installed
	// at all and a "nb-…" host should fall through to chi's normal routing.
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "nb-deadbeef.agent.test"
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d (healthz should answer 200 when vhost disabled)", rr.Code)
	}
}

func TestNotebookSessionURL_VhostShape(t *testing.T) {
	s := &Server{
		NotebookHostBaseDomain:  "agent.test",
		NotebookSubdomainPrefix: "nb",
	}
	got := s.notebookSessionURL("77f66719-aaaa-bbbb-cccc-000000000001", "tok123")
	want := "https://nb-77f66719.agent.test/auth?token=tok123"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestNotebookSessionURL_LegacyShape(t *testing.T) {
	s := &Server{}
	got := s.notebookSessionURL("ws-1", "tok")
	want := "/api/notebooks/ws-1/lab"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}
