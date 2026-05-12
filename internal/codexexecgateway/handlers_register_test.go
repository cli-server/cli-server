package codexexecgateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHandleRegister_HappyPath(t *testing.T) {
	store := newTestStore(t)
	srv, err := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s"}, store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	body := bytes.NewReader([]byte(`{"display_name":"laptop","description":"d","default_cwd":"/x"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/register", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "user_a") // see step 3 — placeholder auth header
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ExeID             string `json:"exe_id"`
		RegistrationToken string `json:"registration_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.ExeID, "exe_") || resp.RegistrationToken == "" {
		t.Fatalf("bad response: %+v", resp)
	}
	// Token round-trip: bcrypt hash from DB must verify against returned token.
	hash, err := store.GetRegistrationTokenHash(req.Context(), resp.ExeID)
	if err != nil || hash == "" {
		t.Fatalf("hash: %v %q", err, hash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(resp.RegistrationToken)); err != nil {
		t.Fatalf("bcrypt verify: %v", err)
	}
}

func TestHandleRegister_RequiresUser(t *testing.T) {
	srv, err := NewServer(Config{CapTokenHMACSecret: []byte("test-hmac"), InternalSharedSecret: "test-internal"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/register", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}
