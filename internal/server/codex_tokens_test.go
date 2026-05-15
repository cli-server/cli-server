package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
	"golang.org/x/crypto/bcrypt"
)

func newCodexTokensTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	d := newCodexTestDBForServer(t)
	srv := &Server{DB: d}
	return srv, d
}

func ctxWithUser(uid string) context.Context {
	return auth.ContextWithUserID(context.Background(), uid)
}

func TestHandleMintCodexToken_HappyPath(t *testing.T) {
	srv, d := newCodexTokensTestServer(t)
	seedWorkspaceMember(t, d, "ws_a", "u1", "owner")

	body := bytes.NewReader([]byte(`{"workspace_id":"ws_a","name":"my mac","ttl_days":30}`))
	req := httptest.NewRequest(http.MethodPost, "/api/codex/tokens", body).
		WithContext(ctxWithUser("u1"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleMintCodexToken(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID, Token, Name, WorkspaceID, ExpiresAt string
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" || len(resp.Token) < 30 {
		t.Fatalf("missing fields: %+v", resp)
	}
	id, secret, err := parseCodexToken(resp.Token)
	if err != nil || id != resp.ID {
		t.Fatalf("token shape: %v id=%q resp.ID=%q", err, id, resp.ID)
	}
	row, _ := d.GetCodexToken(req.Context(), id)
	if row == nil {
		t.Fatal("row missing")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(row.TokenHash), []byte(secret)); err != nil {
		t.Fatalf("hash verify: %v", err)
	}
}

func TestHandleMintCodexToken_NotMember_403(t *testing.T) {
	srv, _ := newCodexTokensTestServer(t)
	body := bytes.NewReader([]byte(`{"workspace_id":"ws_a","name":"x"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/codex/tokens", body).
		WithContext(ctxWithUser("u_no"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleMintCodexToken(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleMintCodexToken_TTLClamp(t *testing.T) {
	srv, d := newCodexTokensTestServer(t)
	seedWorkspaceMember(t, d, "ws_a", "u1", "owner")
	body := bytes.NewReader([]byte(`{"workspace_id":"ws_a","name":"x","ttl_days":99999}`))
	req := httptest.NewRequest(http.MethodPost, "/api/codex/tokens", body).
		WithContext(ctxWithUser("u1"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleMintCodexToken(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", rr.Code)
	}
}

func TestHandleListCodexTokens(t *testing.T) {
	srv, d := newCodexTokensTestServer(t)
	seedWorkspaceMember(t, d, "ws_a", "u1", "owner")
	for _, n := range []string{"a", "b"} {
		body := bytes.NewReader([]byte(`{"workspace_id":"ws_a","name":"` + n + `"}`))
		req := httptest.NewRequest(http.MethodPost, "/api/codex/tokens", body).
			WithContext(ctxWithUser("u1"))
		req.Header.Set("Content-Type", "application/json")
		srv.handleMintCodexToken(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/codex/tokens?workspace_id=ws_a", nil).
		WithContext(ctxWithUser("u1"))
	rr := httptest.NewRecorder()
	srv.handleListCodexTokens(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got []map[string]any
	json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Fatalf("want 2 tokens, got %d: %v", len(got), got)
	}
	if _, ok := got[0]["token"]; ok {
		t.Fatal("list response must NOT include raw token")
	}
}

func TestHandleRevokeCodexToken(t *testing.T) {
	srv, d := newCodexTokensTestServer(t)
	seedWorkspaceMember(t, d, "ws_a", "u1", "owner")
	body := bytes.NewReader([]byte(`{"workspace_id":"ws_a","name":"x"}`))
	mintReq := httptest.NewRequest(http.MethodPost, "/api/codex/tokens", body).
		WithContext(ctxWithUser("u1"))
	mintReq.Header.Set("Content-Type", "application/json")
	mintRR := httptest.NewRecorder()
	srv.handleMintCodexToken(mintRR, mintReq)
	var mr struct{ ID string }
	json.Unmarshal(mintRR.Body.Bytes(), &mr)

	delReq := httptest.NewRequest(http.MethodDelete, "/api/codex/tokens/"+mr.ID, nil).
		WithContext(ctxWithUser("u1"))
	rr := httptest.NewRecorder()
	srv.routesForCodexTokens().ServeHTTP(rr, delReq)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	rr2 := httptest.NewRecorder()
	srv.routesForCodexTokens().ServeHTTP(rr2, delReq)
	if rr2.Code != http.StatusNoContent {
		t.Fatalf("second delete status = %d", rr2.Code)
	}
}
