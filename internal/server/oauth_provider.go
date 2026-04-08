package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/agentserver/agentserver/internal/auth"
)

// handleOAuthLogin is the Hydra login provider endpoint.
// Hydra redirects here with ?login_challenge=xxx.
func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	challenge := r.URL.Query().Get("login_challenge")
	if challenge == "" {
		http.Error(w, "missing login_challenge", http.StatusBadRequest)
		return
	}

	loginReq, err := s.HydraClient.GetLoginRequest(challenge)
	if err != nil {
		log.Printf("oauth login: get login request: %v", err)
		http.Error(w, "failed to get login request", http.StatusInternalServerError)
		return
	}

	// If Hydra says we can skip (user already authenticated), accept immediately.
	if loginReq.Skip {
		redirect, err := s.HydraClient.AcceptLogin(challenge, auth.AcceptLoginBody{
			Subject:  loginReq.Subject,
			Remember: true,
		})
		if err != nil {
			log.Printf("oauth login: accept skip: %v", err)
			http.Error(w, "failed to accept login", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}

	// Check if the user has an existing agentserver session cookie.
	if userID, ok := s.Auth.ValidateRequest(r); ok {
		redirect, err := s.HydraClient.AcceptLogin(challenge, auth.AcceptLoginBody{
			Subject:  userID,
			Remember: true,
		})
		if err != nil {
			log.Printf("oauth login: accept with cookie: %v", err)
			http.Error(w, "failed to accept login", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}

	// No session — redirect to the frontend login page.
	http.Redirect(w, r, "/oauth2/login?login_challenge="+challenge, http.StatusFound)
}

// handleOAuthLoginSubmit processes the login form submission during OAuth flow.
func (s *Server) handleOAuthLoginSubmit(w http.ResponseWriter, r *http.Request) {
	challenge := r.URL.Query().Get("login_challenge")
	if challenge == "" {
		http.Error(w, "missing login_challenge", http.StatusBadRequest)
		return
	}

	userID, ok := s.Auth.ValidateRequest(r)
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	redirect, err := s.HydraClient.AcceptLogin(challenge, auth.AcceptLoginBody{
		Subject:  userID,
		Remember: true,
	})
	if err != nil {
		log.Printf("oauth login submit: accept: %v", err)
		http.Error(w, "failed to accept login", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"redirect_to": redirect})
}

// handleOAuthConsent is the Hydra consent provider endpoint.
// Hydra redirects here with ?consent_challenge=xxx.
func (s *Server) handleOAuthConsent(w http.ResponseWriter, r *http.Request) {
	challenge := r.URL.Query().Get("consent_challenge")
	if challenge == "" {
		http.Error(w, "missing consent_challenge", http.StatusBadRequest)
		return
	}

	// Validate the consent challenge with Hydra.
	if _, err := s.HydraClient.GetConsentRequest(challenge); err != nil {
		log.Printf("oauth consent: get consent request: %v", err)
		http.Error(w, "failed to get consent request", http.StatusInternalServerError)
		return
	}

	// Redirect to frontend consent UI.
	http.Redirect(w, r, "/oauth2/consent?consent_challenge="+challenge, http.StatusFound)
}

// handleOAuthConsentSubmit processes the consent form submission (workspace selection).
func (s *Server) handleOAuthConsentSubmit(w http.ResponseWriter, r *http.Request) {
	challenge := r.URL.Query().Get("consent_challenge")
	if challenge == "" {
		http.Error(w, "missing consent_challenge", http.StatusBadRequest)
		return
	}

	// Verify the caller has a valid session.
	sessionUserID, ok := s.Auth.ValidateRequest(r)
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Action      string `json:"action"` // "accept" or "deny"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.Action == "deny" {
		redirect, err := s.HydraClient.RejectConsent(challenge, auth.RejectBody{
			Error:            "access_denied",
			ErrorDescription: "user denied consent",
		})
		if err != nil {
			log.Printf("oauth consent: reject: %v", err)
			http.Error(w, "failed to reject consent", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"redirect_to": redirect})
		return
	}

	// Get consent request to extract subject.
	consentReq, err := s.HydraClient.GetConsentRequest(challenge)
	if err != nil {
		log.Printf("oauth consent submit: get consent request: %v", err)
		http.Error(w, "failed to get consent request", http.StatusInternalServerError)
		return
	}

	// Verify session user matches the OAuth subject (defense in depth).
	if consentReq.Subject != sessionUserID {
		http.Error(w, "session user does not match OAuth subject", http.StatusForbidden)
		return
	}

	// Verify the user is a developer+ member of the selected workspace.
	role, err := s.DB.GetWorkspaceMemberRole(req.WorkspaceID, consentReq.Subject)
	if err != nil {
		log.Printf("oauth consent submit: check role: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if role == "" || role == "guest" {
		http.Error(w, "insufficient permissions for this workspace", http.StatusForbidden)
		return
	}

	redirect, err := s.HydraClient.AcceptConsent(challenge, auth.AcceptConsentBody{
		GrantScope: consentReq.RequestedScope,
		Session: auth.ConsentSession{
			AccessToken: map[string]interface{}{
				"workspace_id":   req.WorkspaceID,
				"workspace_role": role,
			},
			IDToken: map[string]interface{}{
				"workspace_id": req.WorkspaceID,
			},
		},
	})
	if err != nil {
		log.Printf("oauth consent submit: accept: %v", err)
		http.Error(w, "failed to accept consent", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"redirect_to": redirect})
}

// handleOAuthDeviceAccept accepts a device challenge after the user confirms the user_code.
func (s *Server) handleOAuthDeviceAccept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceChallenge string `json:"device_challenge"`
		UserCode        string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.DeviceChallenge == "" || req.UserCode == "" {
		http.Error(w, "device_challenge and user_code are required", http.StatusBadRequest)
		return
	}

	redirect, err := s.HydraClient.AcceptDeviceChallenge(req.DeviceChallenge, auth.AcceptDeviceBody{
		UserCode: req.UserCode,
	})
	if err != nil {
		log.Printf("oauth device accept: %v", err)
		http.Error(w, "failed to accept device challenge", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"redirect_to": redirect})
}
