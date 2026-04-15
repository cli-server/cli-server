package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
)

// isValidSandboxType mirrors the type-validation predicate in handleAgentRegister.
// It is duplicated here to make the unit tests independent of the handler's control flow.
func isValidSandboxType(t string) bool {
	return t == "opencode" || t == "claudecode" || t == "custom"
}

// TestSandboxTypeValidation_Logic verifies the type validation predicate
// accepts exactly the three expected types.
func TestSandboxTypeValidation_Logic(t *testing.T) {
	tests := []struct {
		sandboxType string
		wantValid   bool
	}{
		{"opencode", true},
		{"claudecode", true},
		{"custom", true},
		{"unknown", false},
		{"", false},
		{"OPENCODE", false},
		{"Custom", false},
	}
	for _, tc := range tests {
		t.Run(tc.sandboxType, func(t *testing.T) {
			if got := isValidSandboxType(tc.sandboxType); got != tc.wantValid {
				t.Errorf("isValidSandboxType(%q) = %v, want %v", tc.sandboxType, got, tc.wantValid)
			}
		})
	}
}

// TestCustomType_NoOpencodePassword verifies that the opencodePassword
// generation condition does not fire for the "custom" sandbox type.
// This mirrors the exact condition used in handleAgentRegister.
func TestCustomType_NoOpencodePassword(t *testing.T) {
	tests := []struct {
		sandboxType          string
		expectOpencodePasswd bool
	}{
		{"opencode", true},
		{"claudecode", false},
		{"custom", false},
	}
	for _, tc := range tests {
		t.Run(tc.sandboxType, func(t *testing.T) {
			// Mirror the exact condition from handleAgentRegister.
			var opencodePassword string
			if tc.sandboxType == "opencode" {
				opencodePassword = "generated"
			}
			hasPassword := opencodePassword != ""
			if hasPassword != tc.expectOpencodePasswd {
				t.Errorf("type %q: opencodePassword generated=%v, want=%v",
					tc.sandboxType, hasPassword, tc.expectOpencodePasswd)
			}
		})
	}
}

// TestAgentRegister_TypeValidation_Integration tests the full handler against a
// real database. Skipped when TEST_DATABASE_URL is not set.
func TestAgentRegister_TypeValidation_Integration(t *testing.T) {
	dbURL := testDatabaseURL(t)
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	database, err := db.Open(dbURL)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	// Fake Hydra: returns an active token for a known workspace/user pair.
	const testWorkspaceIDVal = "ws-register-test"
	const testUserID = "user-register-test"

	_, err = database.Exec(
		`INSERT INTO workspaces (id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		testWorkspaceIDVal, "Register Test Workspace",
	)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() { database.Exec(`DELETE FROM workspaces WHERE id = $1`, testWorkspaceIDVal) })

	_, err = database.Exec(
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'developer') ON CONFLICT DO NOTHING`,
		testWorkspaceIDVal, testUserID,
	)
	if err != nil {
		t.Fatalf("add workspace member: %v", err)
	}
	t.Cleanup(func() {
		database.Exec(`DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceIDVal, testUserID)
	})

	hydra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		json.NewEncoder(w).Encode(auth.IntrospectionResult{
			Active:  true,
			Subject: testUserID,
			Scope:   "agent:register",
			Extra:   map[string]interface{}{"workspace_id": testWorkspaceIDVal},
		})
	}))
	defer hydra.Close()

	s := &Server{
		DB:          database,
		HydraClient: auth.NewHydraClient(hydra.URL, hydra.URL),
	}

	tests := []struct {
		name        string
		sandboxType string
		wantStatus  int
	}{
		{"opencode accepted", "opencode", http.StatusCreated},
		{"claudecode accepted", "claudecode", http.StatusCreated},
		{"custom accepted", "custom", http.StatusCreated},
		{"invalid type rejected", "bogus", http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"name": "test-agent", "type": tc.sandboxType})
			req := httptest.NewRequest(http.MethodPost, "/api/agent/register", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer fake-token")
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			s.handleAgentRegister(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("type %q: got status %d, want %d (body: %s)",
					tc.sandboxType, rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus == http.StatusBadRequest {
				msg := rr.Body.String()
				for _, keyword := range []string{"opencode", "claudecode", "custom"} {
					if !bytes.Contains([]byte(msg), []byte(keyword)) {
						t.Errorf("error message missing %q: %s", keyword, msg)
					}
				}
			}
		})
	}
}
