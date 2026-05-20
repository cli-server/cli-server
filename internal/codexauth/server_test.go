package codexauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestServer_MountRegistersAllRoutes(t *testing.T) {
	s := &Server{IssuerURL: "https://example/codex-auth"}
	r := chi.NewRouter()
	s.Mount(r)

	// Assert each route at least returns non-404 (will return 4xx/405 for
	// missing handlers/wrong methods, but the route must be registered).
	want := []string{
		"/oauth/authorize",
		"/oauth/token",
		"/agent-identities/jwks",
		"/codex/device",
		"/api/accounts/deviceauth/usercode",
		"/api/accounts/deviceauth/token",
	}
	for _, path := range want {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code == http.StatusNotFound {
			t.Errorf("route %s not registered (404)", path)
		}
	}
}
