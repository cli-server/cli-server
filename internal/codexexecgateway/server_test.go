package codexexecgateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServer_HealthZ(t *testing.T) {
	cfg := Config{
		CapTokenHMACSecret:   []byte("test-hmac-key"),
		InternalSharedSecret: "test-internal-secret",
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
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

func TestConfig_Validate_RequiresHMACSecret(t *testing.T) {
	err := Config{InternalSharedSecret: "x"}.Validate()
	if err == nil || !strings.Contains(err.Error(), "CapTokenHMACSecret") {
		t.Fatalf("want HMAC required, got %v", err)
	}
}

func TestConfig_Validate_RequiresInternalSecret(t *testing.T) {
	err := Config{CapTokenHMACSecret: []byte("k")}.Validate()
	if err == nil || !strings.Contains(err.Error(), "InternalSharedSecret") {
		t.Fatalf("want internal-secret required, got %v", err)
	}
}

func TestNewServer_RejectsZeroConfig(t *testing.T) {
	_, err := NewServer(Config{}, nil)
	if err == nil {
		t.Fatal("NewServer should reject zero-value Config")
	}
}
