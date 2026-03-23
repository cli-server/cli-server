package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"
)

var modelserverTokenRefresh singleflight.Group

// getValidModelserverToken returns a valid access token for the given workspace,
// refreshing it if needed. Uses singleflight to avoid thundering-herd on refresh.
func (s *Server) getValidModelserverToken(workspaceID string) (token string, expiresAt time.Time, err error) {
	conn, err := s.DB.GetModelserverConnection(workspaceID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("get modelserver connection: %w", err)
	}
	if conn == nil {
		return "", time.Time{}, fmt.Errorf("no modelserver connection for workspace %s", workspaceID)
	}

	// If token is still valid with 60s buffer, return it immediately.
	if time.Now().Add(60 * time.Second).Before(conn.TokenExpiresAt) {
		return conn.AccessToken, conn.TokenExpiresAt, nil
	}

	// Token is expired or about to expire — refresh via singleflight.
	type result struct {
		token     string
		expiresAt time.Time
	}
	v, doErr, _ := modelserverTokenRefresh.Do(workspaceID, func() (interface{}, error) {
		// Re-check DB: another goroutine may have already refreshed.
		fresh, err := s.DB.GetModelserverConnection(workspaceID)
		if err != nil {
			return nil, fmt.Errorf("re-check modelserver connection: %w", err)
		}
		if fresh == nil {
			return nil, fmt.Errorf("no modelserver connection for workspace %s", workspaceID)
		}
		if time.Now().Add(60 * time.Second).Before(fresh.TokenExpiresAt) {
			return result{token: fresh.AccessToken, expiresAt: fresh.TokenExpiresAt}, nil
		}

		// Still expired — perform the refresh.
		data := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {fresh.RefreshToken},
			"client_id":     {s.ModelserverOAuthClientID},
			"client_secret": {s.ModelserverOAuthClientSecret},
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.PostForm(s.ModelserverOAuthTokenURL, data)
		if err != nil {
			return nil, fmt.Errorf("refresh token request: %w", err)
		}
		defer resp.Body.Close()

		var tokenResp modelserverTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
			return nil, fmt.Errorf("decode refresh token response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("refresh token endpoint returned %d", resp.StatusCode)
		}

		newExpiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

		// Handle refresh token rotation: use new one if returned, otherwise keep the old one.
		newRefreshToken := tokenResp.RefreshToken
		if newRefreshToken == "" {
			newRefreshToken = fresh.RefreshToken
		}

		if err := s.DB.UpdateModelserverTokens(workspaceID, tokenResp.AccessToken, newRefreshToken, newExpiresAt); err != nil {
			return nil, fmt.Errorf("update modelserver tokens: %w", err)
		}

		return result{token: tokenResp.AccessToken, expiresAt: newExpiresAt}, nil
	})

	if doErr != nil {
		return "", time.Time{}, doErr
	}

	res := v.(result)
	return res.token, res.expiresAt, nil
}

// handleInternalModelserverToken returns a valid access token for a workspace's ModelServer connection.
// GET /internal/workspaces/{id}/modelserver-token
func (s *Server) handleInternalModelserverToken(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")

	token, expiresAt, err := s.getValidModelserverToken(wsID)
	if err != nil {
		log.Printf("internal modelserver token: workspace %s: %v", wsID, err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"access_token": token,
		"expires_at":   expiresAt.UTC().Format(time.RFC3339),
	})
}
