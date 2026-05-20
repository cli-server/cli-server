package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Integration test: requires TEST_DATABASE_URL (skipped otherwise).
// Verifies session-open returns a valid session id, sets metadata on
// the row, and session-close marks it disconnected.
func TestHandleCodexSession_OpenThenClose(t *testing.T) {
	srv, d := newCodexTokensTestServer(t)
	mintRow(t, d, "abc12345", "supersecret", "u1", "ws_a", time.Now().Add(time.Hour), nil)

	body := bytes.NewReader([]byte(`{
        "token": "ast_abc12345_supersecret",
        "client_ip": "203.0.113.7",
        "client_ua": "codex_cli_rs/0.130.0 (Darwin 24.4.0; arm64)",
        "codex_version": "0.130.0",
        "os": "Darwin"
    }`))
	req := httptest.NewRequest(http.MethodPost, "/api/internal/codex/tokens/session-open", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleCodexSessionOpen(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("open status = %d body=%s", rr.Code, rr.Body.String())
	}
	var openResp sessionOpenResp
	if err := json.Unmarshal(rr.Body.Bytes(), &openResp); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	if openResp.SessionID == "" || openResp.UserID != "u1" || openResp.WorkspaceID != "ws_a" {
		t.Fatalf("open resp = %+v", openResp)
	}

	// Row exists with metadata + no disconnected_at.
	latest, err := d.LatestCodexBrowserSession(context.Background(), "abc12345")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest == nil || latest.DisconnectedAt != nil {
		t.Fatalf("expected open session, got %+v", latest)
	}
	if latest.ClientIP != "203.0.113.7" || latest.CodexVersion != "0.130.0" || latest.OS != "Darwin" {
		t.Fatalf("metadata not stored: %+v", latest)
	}
	openCount, _ := d.CountOpenCodexBrowserSessions(context.Background(), "abc12345")
	if openCount != 1 {
		t.Fatalf("open count = %d want 1", openCount)
	}

	// Close it.
	closeBody := bytes.NewReader([]byte(`{"session_id":"` + openResp.SessionID + `"}`))
	closeReq := httptest.NewRequest(http.MethodPost, "/api/internal/codex/tokens/session-close", closeBody)
	closeReq.Header.Set("Content-Type", "application/json")
	closeRR := httptest.NewRecorder()
	srv.handleCodexSessionClose(closeRR, closeReq)
	if closeRR.Code != http.StatusNoContent {
		t.Fatalf("close status = %d body=%s", closeRR.Code, closeRR.Body.String())
	}

	openCount, _ = d.CountOpenCodexBrowserSessions(context.Background(), "abc12345")
	if openCount != 0 {
		t.Fatalf("open count after close = %d want 0", openCount)
	}
}

// Session-open with bad secret must 401 — guards against a malicious or
// buggy CXG forging sessions for token ids it doesn't actually hold.
func TestHandleCodexSessionOpen_BadSecret_401(t *testing.T) {
	srv, d := newCodexTokensTestServer(t)
	mintRow(t, d, "abc12345", "rightsecret", "u1", "ws_a", time.Now().Add(time.Hour), nil)
	body := bytes.NewReader([]byte(`{"token":"ast_abc12345_wrongsecret"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/internal/codex/tokens/session-open", body)
	rr := httptest.NewRecorder()
	srv.handleCodexSessionOpen(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rr.Code)
	}
	openCount, _ := d.CountOpenCodexBrowserSessions(context.Background(), "abc12345")
	if openCount != 0 {
		t.Fatalf("session leaked on bad secret: count=%d", openCount)
	}
}

// Closing an unknown session id must be a clean no-op (idempotent).
func TestHandleCodexSessionClose_MissingIsNoOp(t *testing.T) {
	srv, _ := newCodexTokensTestServer(t)
	body := bytes.NewReader([]byte(`{"session_id":"cbs_nonexistent"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/internal/codex/tokens/session-close", body)
	rr := httptest.NewRecorder()
	srv.handleCodexSessionClose(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}
