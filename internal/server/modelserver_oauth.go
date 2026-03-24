package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
)

const (
	modelserverStateCookie = "modelserver-oauth-state"
	modelserverWSIDCookie  = "modelserver-oauth-wsid"
	modelserverPKCECookie  = "modelserver-oauth-pkce"
	modelserverCookieMaxAge = 600 // 10 minutes
)

// handleModelserverConnect initiates the OAuth flow to connect a workspace to a ModelServer project.
// GET /api/workspaces/{id}/modelserver/connect
func (s *Server) handleModelserverConnect(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer") {
		return
	}

	if s.ModelserverOAuthAuthURL == "" {
		http.Error(w, "ModelServer OAuth not configured", http.StatusNotImplemented)
		return
	}

	// Generate state: 16 random bytes -> hex.
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		log.Printf("modelserver connect: failed to generate state: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(stateBytes)

	// PKCE: 32 random bytes -> base64url as code_verifier.
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		log.Printf("modelserver connect: failed to generate PKCE verifier: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	// code_challenge = base64url(SHA256(code_verifier))
	h := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	// Set cookies.
	http.SetCookie(w, &http.Cookie{
		Name:     modelserverStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   modelserverCookieMaxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     modelserverWSIDCookie,
		Value:    wsID,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   modelserverCookieMaxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     modelserverPKCECookie,
		Value:    codeVerifier,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   modelserverCookieMaxAge,
	})

	// Build authorization URL.
	params := url.Values{
		"client_id":             {s.ModelserverOAuthClientID},
		"redirect_uri":          {s.ModelserverOAuthRedirectURI},
		"response_type":         {"code"},
		"scope":                 {"project:inference offline_access"},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"consent"}, // Always show project selection, never skip
	}
	authURL := s.ModelserverOAuthAuthURL + "?" + params.Encode()
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleModelserverCallback processes the OAuth callback from the ModelServer authorization server.
// GET /api/auth/modelserver/callback
func (s *Server) handleModelserverCallback(w http.ResponseWriter, r *http.Request) {
	// Extract cookies.
	stateCookie, err := r.Cookie(modelserverStateCookie)
	if err != nil || stateCookie.Value == "" {
		http.Error(w, "missing oauth state", http.StatusBadRequest)
		return
	}
	wsidCookie, err := r.Cookie(modelserverWSIDCookie)
	if err != nil || wsidCookie.Value == "" {
		http.Error(w, "missing workspace id", http.StatusBadRequest)
		return
	}
	pkceCookie, err := r.Cookie(modelserverPKCECookie)
	if err != nil || pkceCookie.Value == "" {
		http.Error(w, "missing pkce verifier", http.StatusBadRequest)
		return
	}

	wsID := wsidCookie.Value
	codeVerifier := pkceCookie.Value

	// Clear all 3 cookies.
	for _, name := range []string{modelserverStateCookie, modelserverWSIDCookie, modelserverPKCECookie} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
		})
	}

	// Helper to redirect with error.
	redirectError := func(msg string) {
		target := fmt.Sprintf("/workspaces/%s?tab=settings&modelserver=error&message=%s", wsID, url.QueryEscape(msg))
		http.Redirect(w, r, target, http.StatusFound)
	}

	// Validate state.
	if r.URL.Query().Get("state") != stateCookie.Value {
		redirectError("invalid oauth state")
		return
	}

	// Check for error from authorization server.
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		desc := r.URL.Query().Get("error_description")
		log.Printf("modelserver callback error: %s — %s", errParam, desc)
		redirectError("authorization failed: " + errParam)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		redirectError("missing authorization code")
		return
	}

	// Verify user has owner/maintainer role on workspace.
	// Use redirect instead of raw HTTP error since we're in the OAuth callback.
	userID := auth.UserIDFromContext(r.Context())
	member, err := s.DB.GetWorkspaceMember(wsID, userID)
	if err != nil || member == nil || (member.Role != "owner" && member.Role != "maintainer") {
		redirectError("insufficient permissions")
		return
	}

	// Exchange code for tokens.
	tokenResp, err := s.exchangeModelserverCode(code, codeVerifier)
	if err != nil {
		log.Printf("modelserver token exchange failed: %v", err)
		redirectError("token exchange failed")
		return
	}

	// Introspect the access token to get project info.
	projectID, projectName, msUserID, err := s.introspectModelserverToken(tokenResp.AccessToken)
	if err != nil {
		log.Printf("modelserver token introspection failed: %v", err)
		redirectError("token introspection failed")
		return
	}

	// Fetch available models.
	models := s.fetchModelserverModels(tokenResp.AccessToken)

	// Delete existing BYOK config (modelserver supersedes it).
	if err := s.DB.DeleteWorkspaceLLMConfig(wsID); err != nil {
		log.Printf("modelserver callback: failed to delete byok config for workspace %s: %v", wsID, err)
	}

	// Upsert ModelserverConnection.
	conn := &db.ModelserverConnection{
		WorkspaceID:    wsID,
		ProjectID:      projectID,
		ProjectName:    projectName,
		UserID:         msUserID,
		AccessToken:    tokenResp.AccessToken,
		RefreshToken:   tokenResp.RefreshToken,
		TokenExpiresAt: time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		Models:         models,
	}
	if err := s.DB.SetModelserverConnection(conn); err != nil {
		log.Printf("modelserver callback: failed to save connection for workspace %s: %v", wsID, err)
		redirectError("failed to save connection")
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/workspaces/%s?tab=settings&modelserver=connected", wsID), http.StatusFound)
}

// modelserverTokenResponse represents the OAuth token endpoint response.
type modelserverTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (s *Server) exchangeModelserverCode(code, codeVerifier string) (*modelserverTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {s.ModelserverOAuthClientID},
		"client_secret": {s.ModelserverOAuthClientSecret},
		"redirect_uri":  {s.ModelserverOAuthRedirectURI},
		"code_verifier": {codeVerifier},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(s.ModelserverOAuthTokenURL, data)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp modelserverTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &tokenResp, nil
}

func (s *Server) introspectModelserverToken(accessToken string) (projectID, projectName, userID string, err error) {
	data := url.Values{
		"token": {accessToken},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(s.ModelserverOAuthIntrospectURL, data)
	if err != nil {
		return "", "", "", fmt.Errorf("introspect request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", fmt.Errorf("read introspect response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("introspect endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Active bool `json:"active"`
		Ext    struct {
			ProjectID   string `json:"project_id"`
			ProjectName string `json:"project_name"`
			UserID      string `json:"user_id"`
		} `json:"ext"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", "", fmt.Errorf("decode introspect response: %w", err)
	}
	if !result.Active {
		return "", "", "", fmt.Errorf("token is not active")
	}

	return result.Ext.ProjectID, result.Ext.ProjectName, result.Ext.UserID, nil
}

func (s *Server) fetchModelserverModels(accessToken string) []db.LLMModel {
	if s.ModelserverProxyURL == "" {
		return nil
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", strings.TrimRight(s.ModelserverProxyURL, "/")+"/v1/models", nil)
	if err != nil {
		log.Printf("modelserver fetch models: failed to create request: %v", err)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("modelserver fetch models: request failed: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("modelserver fetch models: returned status %d", resp.StatusCode)
		return nil
	}

	var result struct {
		Data []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("modelserver fetch models: decode failed: %v", err)
		return nil
	}

	models := make([]db.LLMModel, len(result.Data))
	for i, name := range result.Data {
		models[i] = db.LLMModel{ID: name, Name: name}
	}
	return models
}

// handleModelserverDisconnect removes the ModelServer connection for a workspace.
// DELETE /api/workspaces/{id}/modelserver/disconnect
func (s *Server) handleModelserverDisconnect(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer") {
		return
	}

	if err := s.DB.DeleteModelserverConnection(wsID); err != nil {
		log.Printf("modelserver disconnect: failed for workspace %s: %v", wsID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleModelserverStatus returns the ModelServer connection status for a workspace.
// GET /api/workspaces/{id}/modelserver/status
func (s *Server) handleModelserverStatus(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	conn, err := s.DB.GetModelserverConnection(wsID)
	if err != nil {
		log.Printf("modelserver status: failed for workspace %s: %v", wsID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if conn == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": false,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected":    true,
		"project_id":   conn.ProjectID,
		"project_name": conn.ProjectName,
		"models":       conn.Models,
		"connected_at": conn.CreatedAt.Format(time.RFC3339),
	})
}
