package codexexecgateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRevokeTurn_AddsToSet(t *testing.T) {
	store := newTestStore(t)
	srv, err := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "secret"}, store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)

	body := bytes.NewReader([]byte(`{"turn_id":"trn_42","exp":9999999999}`))
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/exec-gateway/revoke-turn", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if !srv.revoked.Contains("trn_42") {
		t.Fatal("revoked set should contain trn_42")
	}
}

func TestRevokeTurn_RejectsBadAuth(t *testing.T) {
	srv, err := NewServer(Config{CapTokenHMACSecret: []byte("test-hmac"), InternalSharedSecret: "test-internal"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/exec-gateway/revoke-turn",
		bytes.NewReader([]byte(`{"turn_id":"x","exp":1}`)))
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestRevokeTurn_BadJSON(t *testing.T) {
	srv, err := NewServer(Config{CapTokenHMACSecret: []byte("test-hmac"), InternalSharedSecret: "test-internal"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/exec-gateway/revoke-turn",
		bytes.NewReader([]byte(`!`)))
	req.Header.Set("Authorization", "Bearer test-internal")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}
