package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/agentserver/agentserver/internal/db"
)

// operationsRouter wires the /internal/operations routes with the same
// X-Internal-Secret check used in production (server.go), so tests cover
// both the auth gate and the handler bodies.
func operationsRouter(s *Server) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/internal/operations", func(w http.ResponseWriter, req *http.Request) {
		secret := os.Getenv("INTERNAL_API_SECRET")
		if secret != "" {
			if req.Header.Get("X-Internal-Secret") != secret {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		s.postInternalOperations(w, req)
	})
	r.Get("/internal/operations", func(w http.ResponseWriter, req *http.Request) {
		secret := os.Getenv("INTERNAL_API_SECRET")
		if secret != "" {
			if req.Header.Get("X-Internal-Secret") != secret {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		s.getInternalOperations(w, req)
	})
	return r
}

func TestPostInternalOperations_Unauthorized(t *testing.T) {
	os.Setenv("INTERNAL_API_SECRET", "opsecret")
	defer os.Unsetenv("INTERNAL_API_SECRET")

	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/internal/operations",
		bytes.NewReader([]byte(`{}`)))
	// no X-Internal-Secret header
	rr := httptest.NewRecorder()
	operationsRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rr.Code)
	}
}

func TestGetInternalOperations_RequiresWorkspaceID(t *testing.T) {
	// No INTERNAL_API_SECRET set, so auth is bypassed; handler must still
	// reject missing workspace_id with 400.
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/internal/operations", nil)
	rr := httptest.NewRecorder()
	operationsRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
}

func TestPostInternalOperations_WritesRow(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	id := uuid.NewString()
	ws := "ws_post_" + t.Name()
	now := time.Now().UTC()
	body := map[string]any{
		"id":             id,
		"workspace_id":   ws,
		"source":         "sdk",
		"env_id":         "alpha",
		"tool":           "shell",
		"arguments":      map[string]any{"command": "ls"},
		"is_error":       false,
		"result_summary": "ok",
		"started_at":     now.Format(time.RFC3339Nano),
		"completed_at":   now.Add(8 * time.Millisecond).Format(time.RFC3339Nano),
		"duration_ms":    8,
	}
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM operations WHERE workspace_id=$1`, ws)
	})

	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/internal/operations",
		bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	operationsRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}

	// Verify the row landed by listing it back.
	rows, err := s.DB.ListOperations(db.OperationFilter{WorkspaceID: ws})
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("rows = %+v, want 1 with id=%s", rows, id)
	}
}

func TestGetInternalOperations_FiltersByWorkspaceAndTool(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	ws := "ws_get_a_" + t.Name()
	other := "ws_get_b_" + t.Name()
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM operations WHERE workspace_id IN ($1,$2)`, ws, other)
	})

	post := func(wsID, env, tool string) {
		now := time.Now().UTC()
		body := map[string]any{
			"id":           uuid.NewString(),
			"workspace_id": wsID,
			"source":       "sdk",
			"env_id":       env,
			"tool":         tool,
			"arguments":    map[string]any{},
			"is_error":     false,
			"started_at":   now.Format(time.RFC3339Nano),
			"completed_at": now.Format(time.RFC3339Nano),
			"duration_ms":  1,
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/internal/operations",
			bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		operationsRouter(s).ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("post: %d %s", rr.Code, rr.Body.String())
		}
	}
	post(ws, "alpha", "shell")
	post(ws, "alpha", "read_file")
	post(other, "alpha", "shell")

	req := httptest.NewRequest(http.MethodGet,
		"/internal/operations?workspace_id="+ws+"&tool=shell&limit=50", nil)
	rr := httptest.NewRecorder()
	operationsRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Operations []map[string]any `json:"operations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Operations) != 1 {
		t.Fatalf("rows = %d, want 1; body=%s", len(resp.Operations), rr.Body.String())
	}
	if resp.Operations[0]["workspace_id"] != ws || resp.Operations[0]["tool"] != "shell" {
		t.Fatalf("row = %+v", resp.Operations[0])
	}
}
