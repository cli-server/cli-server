package ccbroker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestListSessionTurns(t *testing.T) {
	store := newFakeStore()
	store.sessions["sess_l"] = &Session{ID: "sess_l", WorkspaceID: "ws"}
	for i := 0; i < 3; i++ {
		_ = store.EnqueueTurn(context.Background(), AgentTurn{
			ID: "trn_l_" + string(rune('a'+i)), SessionID: "sess_l",
			WorkspaceID: "ws", UserEventID: "u", UserMessage: "x",
		})
	}
	srv := &Server{store: store,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := chi.NewRouter()
	r.Get("/api/sessions/{sid}/turns", srv.handleListSessionTurns)
	req := httptest.NewRequest("GET", "/api/sessions/sess_l/turns", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Turns []map[string]interface{} `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(resp.Turns))
	}
}
