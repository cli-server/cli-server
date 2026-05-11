package codexexecgateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServer_HealthZ(t *testing.T) {
	srv := NewServer(Config{}, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: want 200, got %d", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Fatalf("healthz body: want ok, got %q", rr.Body.String())
	}
}
