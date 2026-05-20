package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestDeviceUserCode_ReturnsPendingRow(t *testing.T) {
	srv := newAuthTestServer(t, "")
	r := chi.NewRouter()
	srv.Mount(r)

	body := bytes.NewReader([]byte(`{"client_id":"app_EMoamEEZ73f0CkXaXp7hrann"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/deviceauth/usercode", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     int    `json:"interval"`
	}
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.DeviceAuthID == "" || resp.UserCode == "" || resp.Interval == 0 {
		t.Errorf("missing fields: %+v", resp)
	}
}

func TestDeviceToken_PendingReturns403(t *testing.T) {
	srv := newAuthTestServer(t, "")
	ctx := context.Background()
	srv.Store.InsertDeviceCode(ctx, DeviceCode{
		DeviceAuthID:      "dev-pending",
		UserCode:          "PEND-PEND",
		CodeChallenge:     "c",
		CodeVerifier:      "v",
		AuthorizationCode: "a",
		Status:            "pending",
		ExpiresAt:         time.Now().Add(15 * time.Minute),
	})

	body, _ := json.Marshal(map[string]string{"device_auth_id": "dev-pending", "user_code": "PEND-PEND"})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/deviceauth/token", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	srv.Mount(r)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestDeviceToken_ApprovedReturnsAuthCode(t *testing.T) {
	srv := newAuthTestServer(t, "")
	ctx := context.Background()
	uid := mustCreateTestUser(t, srv.Store.db)
	srv.Store.InsertDeviceCode(ctx, DeviceCode{
		DeviceAuthID:      "dev-ok",
		UserCode:          "GOOD-CODE",
		CodeChallenge:     "ch-1",
		CodeVerifier:      "ver-1",
		AuthorizationCode: "ac-1",
		Status:            "pending",
		ExpiresAt:         time.Now().Add(15 * time.Minute),
	})
	srv.Store.ApproveDeviceCode(ctx, "GOOD-CODE", uid)

	body, _ := json.Marshal(map[string]string{"device_auth_id": "dev-ok", "user_code": "GOOD-CODE"})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/deviceauth/token", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	srv.Mount(r)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeChallenge     string `json:"code_challenge"`
		CodeVerifier      string `json:"code_verifier"`
	}
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.AuthorizationCode != "ac-1" || resp.CodeChallenge != "ch-1" || resp.CodeVerifier != "ver-1" {
		t.Errorf("got = %+v", resp)
	}

	// AND the device row should now have a matching pkce_requests row,
	// so the subsequent /oauth/token call works.
	var n int
	srv.Store.db.QueryRow(`SELECT COUNT(*) FROM codex_pkce_requests WHERE code = 'ac-1'`).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 codex_pkce_requests row, got %d", n)
	}
	_ = strings.TrimSpace // keep "strings" import quiet if unused elsewhere
}

func TestDeviceVerify_UnauthRedirectsToLogin(t *testing.T) {
	srv := newAuthTestServer(t, "")
	r := chi.NewRouter()
	srv.Mount(r)
	req := httptest.NewRequest(http.MethodGet, "/codex/device", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
}

func TestDeviceVerify_AuthedRendersForm(t *testing.T) {
	srv := newAuthTestServer(t, "user-abc")
	r := chi.NewRouter()
	srv.Mount(r)
	req := httptest.NewRequest(http.MethodGet, "/codex/device", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `name="user_code"`) {
		t.Errorf("page missing user_code input")
	}
	if !strings.Contains(body, `name="action"`) {
		t.Errorf("page missing action buttons")
	}
}

func TestDeviceVerify_SubmitApproveFlipsStatus(t *testing.T) {
	srv := newAuthTestServer(t, "user-approver")
	ctx := context.Background()
	srv.Store.InsertDeviceCode(ctx, DeviceCode{
		DeviceAuthID: "dev-form-1", UserCode: "FORM-CODE",
		CodeChallenge: "c", CodeVerifier: "v", AuthorizationCode: "a",
		Status: "pending", ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	form := url.Values{}
	form.Set("user_code", "FORM-CODE")
	form.Set("action", "approve")
	req := httptest.NewRequest(http.MethodPost, "/codex/device", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	srv.Mount(r)
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	row, _ := srv.Store.GetDeviceCodeByUserCode(ctx, "FORM-CODE")
	if row == nil || row.Status != "approved" || row.UserID != "user-approver" {
		t.Errorf("row = %+v", row)
	}
}
