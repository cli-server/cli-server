package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHydraClient_GetLoginRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/oauth2/auth/requests/login" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("login_challenge") != "test-challenge" {
			t.Errorf("missing login_challenge param")
		}
		json.NewEncoder(w).Encode(LoginRequest{
			Challenge:      "test-challenge",
			Subject:        "user-123",
			Skip:           true,
			RequestedScope: []string{"openid", "profile"},
		})
	}))
	defer srv.Close()

	client := NewHydraClient(srv.URL, "https://public.example.com")
	req, err := client.GetLoginRequest("test-challenge")
	if err != nil {
		t.Fatalf("GetLoginRequest: %v", err)
	}
	if req.Challenge != "test-challenge" {
		t.Errorf("Challenge = %q, want %q", req.Challenge, "test-challenge")
	}
	if req.Subject != "user-123" {
		t.Errorf("Subject = %q, want %q", req.Subject, "user-123")
	}
	if !req.Skip {
		t.Error("expected Skip=true")
	}
}

func TestHydraClient_AcceptLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if r.URL.Path != "/admin/oauth2/auth/requests/login/accept" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body AcceptLoginBody
		json.NewDecoder(r.Body).Decode(&body)
		if body.Subject != "user-123" {
			t.Errorf("Subject = %q, want %q", body.Subject, "user-123")
		}
		json.NewEncoder(w).Encode(RedirectResponse{RedirectTo: "https://hydra/callback"})
	}))
	defer srv.Close()

	client := NewHydraClient(srv.URL, "https://public.example.com")
	redirect, err := client.AcceptLogin("test-challenge", AcceptLoginBody{
		Subject:  "user-123",
		Remember: true,
	})
	if err != nil {
		t.Fatalf("AcceptLogin: %v", err)
	}
	if redirect != "https://hydra/callback" {
		t.Errorf("redirect = %q, want %q", redirect, "https://hydra/callback")
	}
}

func TestHydraClient_GetConsentRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/oauth2/auth/requests/consent" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(ConsentRequest{
			Challenge:      "consent-challenge",
			Subject:        "user-123",
			RequestedScope: []string{"openid", "agent:register"},
		})
	}))
	defer srv.Close()

	client := NewHydraClient(srv.URL, "https://public.example.com")
	req, err := client.GetConsentRequest("consent-challenge")
	if err != nil {
		t.Fatalf("GetConsentRequest: %v", err)
	}
	if req.Subject != "user-123" {
		t.Errorf("Subject = %q, want %q", req.Subject, "user-123")
	}
}

func TestHydraClient_AcceptConsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		var body AcceptConsentBody
		json.NewDecoder(r.Body).Decode(&body)
		wsID, _ := body.Session.AccessToken["workspace_id"].(string)
		if wsID != "ws-001" {
			t.Errorf("workspace_id = %q, want %q", wsID, "ws-001")
		}
		json.NewEncoder(w).Encode(RedirectResponse{RedirectTo: "https://hydra/done"})
	}))
	defer srv.Close()

	client := NewHydraClient(srv.URL, "https://public.example.com")
	redirect, err := client.AcceptConsent("consent-challenge", AcceptConsentBody{
		GrantScope: []string{"openid", "agent:register"},
		Session: ConsentSession{
			AccessToken: map[string]interface{}{
				"workspace_id":   "ws-001",
				"workspace_role": "developer",
			},
		},
	})
	if err != nil {
		t.Fatalf("AcceptConsent: %v", err)
	}
	if redirect != "https://hydra/done" {
		t.Errorf("redirect = %q", redirect)
	}
}

func TestHydraClient_IntrospectToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/admin/oauth2/introspect" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		r.ParseForm()
		if r.PostForm.Get("token") != "test-token" {
			t.Errorf("missing token in form")
		}
		json.NewEncoder(w).Encode(IntrospectionResult{
			Active:  true,
			Subject: "user-123",
			Scope:   "openid profile agent:register",
			Extra: map[string]interface{}{
				"workspace_id": "ws-001",
			},
		})
	}))
	defer srv.Close()

	client := NewHydraClient(srv.URL, "https://public.example.com")
	result, err := client.IntrospectToken("test-token")
	if err != nil {
		t.Fatalf("IntrospectToken: %v", err)
	}
	if !result.Active {
		t.Error("expected Active=true")
	}
	if result.Subject != "user-123" {
		t.Errorf("Subject = %q", result.Subject)
	}
	wsID, _ := result.Extra["workspace_id"].(string)
	if wsID != "ws-001" {
		t.Errorf("workspace_id = %q", wsID)
	}
	if !result.HasScope("agent:register") {
		t.Error("expected HasScope(agent:register) = true")
	}
	if result.HasScope("admin") {
		t.Error("expected HasScope(admin) = false")
	}
}

func TestHydraClient_RejectLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		json.NewEncoder(w).Encode(RedirectResponse{RedirectTo: "https://hydra/reject"})
	}))
	defer srv.Close()

	client := NewHydraClient(srv.URL, "https://public.example.com")
	redirect, err := client.RejectLogin("challenge", RejectBody{Error: "access_denied", ErrorDescription: "user cancelled"})
	if err != nil {
		t.Fatalf("RejectLogin: %v", err)
	}
	if redirect != "https://hydra/reject" {
		t.Errorf("redirect = %q", redirect)
	}
}
