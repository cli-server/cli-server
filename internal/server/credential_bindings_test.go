package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/agentserver/agentserver/internal/credentialproxy/provider"
)

// fakeProvider is a minimal provider.Provider for testing the CRUD handlers.
type fakeProvider struct{}

func (p *fakeProvider) Kind() string { return "fake" }

func (p *fakeProvider) ParseUpload(_ string, raw []byte) (*provider.UploadResult, error) {
	return &provider.UploadResult{
		DisplayName: "parsed-name",
		ServerURL:   "https://example.com",
		PublicMeta:  map[string]any{"key": "value"},
		AuthType:    "bearer",
		AuthSecret:  []byte(`{"token":"secret"}`),
	}, nil
}

func (p *fakeProvider) BuildSandboxConfig(
	_ []*provider.BindingMeta, _ string, _ string,
) ([]*provider.SandboxConfigFile, error) {
	return nil, nil
}

func (p *fakeProvider) ServeHTTP(http.ResponseWriter, *http.Request, *provider.DecryptedBinding) {}

func init() {
	// Register the fake provider so handlers can look it up.
	provider.Register("fake", &fakeProvider{})
}

// newTestRouter wires a chi router with the credential binding routes under a
// controlled Server. The caller can override fields on the returned Server
// before making requests.
func newTestRouter(s *Server) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/workspaces/{id}/credentials/{kind}", s.handleListCredentialBindings)
	r.Post("/api/workspaces/{id}/credentials/{kind}", s.handleCreateCredentialBinding)
	r.Patch("/api/workspaces/{id}/credentials/{kind}/{bindingId}", s.handlePatchCredentialBinding)
	r.Delete("/api/workspaces/{id}/credentials/{kind}/{bindingId}", s.handleDeleteCredentialBinding)
	r.Post("/api/workspaces/{id}/credentials/{kind}/{bindingId}/set-default", s.handleSetDefaultCredentialBinding)
	return r
}

// --- Unit tests: validation paths that do not require a database ---

func TestCreateCredentialBinding_NoEncryptionKey(t *testing.T) {
	s := &Server{} // EncryptionKey is zero-value (nil)
	router := newTestRouter(s)

	body := `{"display_name":"test","config":"yaml"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/ws1/credentials/fake", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateCredentialBinding_BadJSON(t *testing.T) {
	s := &Server{EncryptionKey: make([]byte, 32)}
	router := newTestRouter(s)

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/ws1/credentials/fake", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateCredentialBinding_MissingFields(t *testing.T) {
	s := &Server{EncryptionKey: make([]byte, 32)}
	router := newTestRouter(s)

	tests := []struct {
		name string
		body string
	}{
		{"empty display_name", `{"display_name":"","config":"yaml"}`},
		{"empty config", `{"display_name":"test","config":""}`},
		{"both empty", `{"display_name":"","config":""}`},
		{"missing fields", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/workspaces/ws1/credentials/fake", bytes.NewBufferString(tt.body))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateCredentialBinding_UnknownProvider(t *testing.T) {
	s := &Server{EncryptionKey: make([]byte, 32)}
	router := newTestRouter(s)

	body := `{"display_name":"test","config":"yaml"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/ws1/credentials/nosuchkind", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListCredentialBindings_NilDB(t *testing.T) {
	// When DB is nil the handler should panic or return 500. This tests that
	// the route is wired correctly and the handler is reached.
	s := &Server{}
	router := newTestRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/ws1/credentials/fake", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r == nil {
			// If it didn't panic, it should be a 500.
			if w.Code != http.StatusInternalServerError {
				t.Errorf("expected 500 or panic with nil DB, got %d", w.Code)
			}
		}
	}()
	router.ServeHTTP(w, req)
}

// --- Integration tests: full CRUD against a real PostgreSQL ---
// These run only when a TEST_DATABASE_URL is set (e.g. in CI or with docker-compose).

func setupTestDB(t *testing.T) *Server {
	t.Helper()

	dbURL := testDatabaseURL(t)
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	database, err := openTestDB(dbURL)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	encKey := make([]byte, 32) // all-zero key, fine for testing
	return &Server{
		DB:            database,
		EncryptionKey: encKey,
	}
}

func TestCredentialBindings_CreateAndList(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	// Create first binding.
	body := `{"display_name":"my-cluster","config":"apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://example.com\n    certificate-authority-data: dGVzdA==\n  name: test\nusers:\n- name: test\n  user:\n    token: mytoken\ncontexts:\n- context:\n    cluster: test\n    user: test\n  name: test\ncurrent-context: test\n"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var created map[string]interface{}
	json.NewDecoder(w.Body).Decode(&created)
	if created["display_name"] != "my-cluster" {
		t.Errorf("display_name = %v, want %q", created["display_name"], "my-cluster")
	}
	if created["is_default"] != true {
		t.Errorf("first binding should be is_default=true, got %v", created["is_default"])
	}
	bindingID := created["id"].(string)

	// List bindings.
	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/"+wsID+"/credentials/fake", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var listed []map[string]interface{}
	json.NewDecoder(w.Body).Decode(&listed)
	if len(listed) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(listed))
	}
	if listed[0]["id"] != bindingID {
		t.Errorf("listed id = %v, want %v", listed[0]["id"], bindingID)
	}
}

func TestCredentialBindings_SecondBindingNotDefault(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	// Create first (becomes default).
	body1 := `{"display_name":"cluster-a","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body1))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create first: %d: %s", w.Code, w.Body.String())
	}

	// Create second (should NOT be default).
	body2 := `{"display_name":"cluster-b","config":"dummy"}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body2))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create second: %d: %s", w.Code, w.Body.String())
	}

	var second map[string]interface{}
	json.NewDecoder(w.Body).Decode(&second)
	if second["is_default"] != false {
		t.Errorf("second binding should be is_default=false, got %v", second["is_default"])
	}
}

func TestCredentialBindings_DeleteDefaultWithOthersReturns409(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	// Create two bindings.
	body1 := `{"display_name":"default-cluster","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body1))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create first: %d: %s", w.Code, w.Body.String())
	}
	var first map[string]interface{}
	json.NewDecoder(w.Body).Decode(&first)
	defaultID := first["id"].(string)

	body2 := `{"display_name":"other-cluster","config":"dummy"}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body2))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create second: %d: %s", w.Code, w.Body.String())
	}

	// Try to delete the default → 409.
	req = httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+wsID+"/credentials/fake/"+defaultID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("delete default with others: expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCredentialBindings_DeleteLastDefault(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	// Create one binding (becomes default).
	body := `{"display_name":"only-cluster","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d: %s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	json.NewDecoder(w.Body).Decode(&created)
	bindingID := created["id"].(string)

	// Delete the only (default) binding → 204.
	req = httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+wsID+"/credentials/fake/"+bindingID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("delete last default: expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCredentialBindings_DeleteNonDefault(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	// Create two bindings.
	body1 := `{"display_name":"default-c","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body1))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create first: %d", w.Code)
	}

	body2 := `{"display_name":"nondefault-c","config":"dummy"}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body2))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create second: %d", w.Code)
	}
	var second map[string]interface{}
	json.NewDecoder(w.Body).Decode(&second)
	nonDefaultID := second["id"].(string)

	// Delete non-default → 204.
	req = httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+wsID+"/credentials/fake/"+nonDefaultID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("delete non-default: expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCredentialBindings_DeleteWrongWorkspace(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	// Create a binding in wsID.
	body := `{"display_name":"cluster-x","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}
	var created map[string]interface{}
	json.NewDecoder(w.Body).Decode(&created)
	bindingID := created["id"].(string)

	// Delete from a different workspace → 404.
	otherWS := testWorkspaceID(t, s)
	req = httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+otherWS+"/credentials/fake/"+bindingID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("delete wrong workspace: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCredentialBindings_SetDefault(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	// Create two bindings.
	body1 := `{"display_name":"first","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body1))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create first: %d", w.Code)
	}

	body2 := `{"display_name":"second","config":"dummy"}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body2))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create second: %d", w.Code)
	}
	var second map[string]interface{}
	json.NewDecoder(w.Body).Decode(&second)
	secondID := second["id"].(string)

	// Set second as default → 204.
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake/"+secondID+"/set-default", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("set-default: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify via list: second should be default.
	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/"+wsID+"/credentials/fake", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var listed []map[string]interface{}
	json.NewDecoder(w.Body).Decode(&listed)
	for _, b := range listed {
		isDefault := b["is_default"].(bool)
		if b["id"] == secondID && !isDefault {
			t.Error("second binding should now be default")
		}
		if b["id"] != secondID && isDefault {
			t.Error("first binding should no longer be default")
		}
	}
}

func TestCredentialBindings_PatchDisplayName(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	// Create a binding.
	body := `{"display_name":"original","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d: %s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	json.NewDecoder(w.Body).Decode(&created)
	bindingID := created["id"].(string)

	// Patch display_name → 204.
	patch := `{"display_name":"renamed"}`
	req = httptest.NewRequest(http.MethodPatch, "/api/workspaces/"+wsID+"/credentials/fake/"+bindingID, bytes.NewBufferString(patch))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("patch: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify via list.
	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/"+wsID+"/credentials/fake", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var listed []map[string]interface{}
	json.NewDecoder(w.Body).Decode(&listed)
	if len(listed) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(listed))
	}
	if listed[0]["display_name"] != "renamed" {
		t.Errorf("display_name = %v, want %q", listed[0]["display_name"], "renamed")
	}
}

func TestCredentialBindings_PatchNotFound(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	patch := `{"display_name":"renamed"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/workspaces/"+wsID+"/credentials/fake/nonexistent", bytes.NewBufferString(patch))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("patch nonexistent: expected 404, got %d", w.Code)
	}
}

func TestCredentialBindings_DuplicateDisplayName(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	body := `{"display_name":"dup-name","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create first: %d", w.Code)
	}

	// Same display_name → 409.
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("duplicate display_name: expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCredentialBindings_ListEmpty(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+wsID+"/credentials/fake", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list empty: expected 200, got %d", w.Code)
	}
	var listed []interface{}
	json.NewDecoder(w.Body).Decode(&listed)
	if len(listed) != 0 {
		t.Errorf("expected 0 bindings, got %d", len(listed))
	}
}

func TestCredentialBindings_ResponseNeverContainsAuthBlob(t *testing.T) {
	s := setupTestDB(t)
	router := newTestRouter(s)
	wsID := testWorkspaceID(t, s)

	body := `{"display_name":"secret-check","config":"dummy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/credentials/fake", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}

	// Create response should not contain auth_blob or the raw secret.
	respBody := w.Body.String()
	for _, forbidden := range []string{"auth_blob", "auth_secret", `"token"`, "secret"} {
		if bytes.Contains([]byte(respBody), []byte(forbidden)) {
			t.Errorf("create response contains forbidden string %q: %s", forbidden, respBody)
		}
	}

	// List response also should not leak secrets.
	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/"+wsID+"/credentials/fake", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	listBody := w.Body.String()
	for _, forbidden := range []string{"auth_blob", "auth_secret"} {
		if bytes.Contains([]byte(listBody), []byte(forbidden)) {
			t.Errorf("list response contains forbidden string %q: %s", forbidden, listBody)
		}
	}
}
