package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	oauth2github "golang.org/x/oauth2/github"
)

// Provider abstracts an OAuth2/OIDC identity provider.
type Provider interface {
	Name() string
	OAuth2Config() *oauth2.Config
	GetIdentity(ctx context.Context, token *oauth2.Token) (subject, email, displayName string, err error)
}

// OIDCManager orchestrates multiple OIDC/OAuth2 providers.
type OIDCManager struct {
	providers map[string]Provider
	baseURL   string
	auth      *Auth
}

// NewOIDCManager creates a new manager. baseURL is the external redirect base (e.g. "https://app.example.com").
func NewOIDCManager(baseURL string, authSvc *Auth) *OIDCManager {
	return &OIDCManager{
		providers: make(map[string]Provider),
		baseURL:   strings.TrimRight(baseURL, "/"),
		auth:      authSvc,
	}
}

// RegisterProvider adds a provider.
func (m *OIDCManager) RegisterProvider(p Provider) {
	m.providers[p.Name()] = p
}

// ProviderNames returns the list of registered provider names.
func (m *OIDCManager) ProviderNames() []string {
	names := make([]string, 0, len(m.providers))
	for n := range m.providers {
		names = append(names, n)
	}
	return names
}

// HandleProviders returns the list of available providers as JSON.
func (m *OIDCManager) HandleProviders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"providers": m.ProviderNames(),
	})
}

const (
	stateCookieName = "cli-server-oauth-state"
	stateCookieTTL  = 10 * time.Minute
)

// HandleLogin redirects the user to the IdP authorization endpoint.
func (m *OIDCManager) HandleLogin(w http.ResponseWriter, r *http.Request, providerName string) {
	p, ok := m.providers[providerName]
	if !ok {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateCookieTTL.Seconds()),
	})

	cfg := p.OAuth2Config()
	http.Redirect(w, r, cfg.AuthCodeURL(state), http.StatusFound)
}

// HandleCallback processes the IdP callback, resolves/creates the user, and sets the auth cookie.
func (m *OIDCManager) HandleCallback(w http.ResponseWriter, r *http.Request, providerName string) {
	p, ok := m.providers[providerName]
	if !ok {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	// Verify state.
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" {
		http.Error(w, "missing oauth state", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	// Clear state cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})

	// Check for error from IdP.
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		desc := r.URL.Query().Get("error_description")
		log.Printf("OIDC callback error from %s: %s — %s", providerName, errParam, desc)
		http.Error(w, "authentication failed: "+errParam, http.StatusUnauthorized)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	// Exchange code for token.
	cfg := p.OAuth2Config()
	token, err := cfg.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("OIDC token exchange failed for %s: %v", providerName, err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	// Get identity from provider.
	subject, email, displayName, err := p.GetIdentity(r.Context(), token)
	if err != nil {
		log.Printf("OIDC get identity failed for %s: %v", providerName, err)
		http.Error(w, "failed to get identity", http.StatusInternalServerError)
		return
	}

	// Resolve or create user.
	userID, err := m.resolveUser(providerName, subject, email, displayName)
	if err != nil {
		log.Printf("OIDC resolve user failed for %s: %v", providerName, err)
		http.Error(w, "failed to resolve user", http.StatusInternalServerError)
		return
	}

	// Issue session token.
	authToken, err := m.auth.IssueToken(userID)
	if err != nil {
		log.Printf("OIDC issue token failed: %v", err)
		http.Error(w, "failed to issue token", http.StatusInternalServerError)
		return
	}

	SetTokenCookie(w, authToken)
	http.Redirect(w, r, "/", http.StatusFound)
}

// resolveUser finds or creates a user for the given OIDC identity.
func (m *OIDCManager) resolveUser(provider, subject, email, displayName string) (string, error) {
	database := m.auth.DB()

	// 1. Check if this OIDC identity is already linked.
	oi, err := database.GetOIDCIdentity(provider, subject)
	if err != nil {
		return "", fmt.Errorf("lookup oidc identity: %w", err)
	}
	if oi != nil {
		// Update email on the identity if it changed.
		return oi.UserID, nil
	}

	// 2. Try matching by email.
	if email != "" {
		user, err := database.GetUserByEmail(email)
		if err != nil {
			return "", fmt.Errorf("lookup user by email: %w", err)
		}
		if user != nil {
			// Link this OIDC identity to the existing user.
			emailPtr := &email
			if err := database.CreateOIDCIdentity(provider, subject, user.ID, emailPtr); err != nil {
				return "", fmt.Errorf("link oidc identity: %w", err)
			}
			return user.ID, nil
		}
	}

	// 3. Create a new user.
	userID := uuid.New().String()
	username := sanitizeUsername(displayName, userID)
	var emailPtr *string
	if email != "" {
		emailPtr = &email
	}
	if err := database.CreateUserWithEmail(userID, username, nil, emailPtr); err != nil {
		return "", fmt.Errorf("create user: %w", err)
	}
	if err := database.CreateOIDCIdentity(provider, subject, userID, emailPtr); err != nil {
		return "", fmt.Errorf("create oidc identity: %w", err)
	}
	return userID, nil
}

// sanitizeUsername generates a safe username from the display name.
func sanitizeUsername(displayName, fallbackID string) string {
	name := strings.TrimSpace(displayName)
	if name == "" {
		name = "user-" + fallbackID[:8]
	}
	// Replace spaces with hyphens, keep alphanumeric and hyphens.
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteRune('-')
		}
	}
	result := b.String()
	if result == "" {
		result = "user-" + fallbackID[:8]
	}
	return result
}

// --- GitHub Provider ---

type GitHubProvider struct {
	clientID     string
	clientSecret string
	redirectURL  string
}

func NewGitHubProvider(clientID, clientSecret, redirectURL string) *GitHubProvider {
	return &GitHubProvider{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirectURL,
	}
}

func (g *GitHubProvider) Name() string { return "github" }

func (g *GitHubProvider) OAuth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     g.clientID,
		ClientSecret: g.clientSecret,
		Endpoint:     oauth2github.Endpoint,
		RedirectURL:  g.redirectURL,
		Scopes:       []string{"user:email"},
	}
}

type githubUser struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func (g *GitHubProvider) GetIdentity(ctx context.Context, token *oauth2.Token) (string, string, string, error) {
	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(token))

	// Get user profile.
	resp, err := client.Get("https://api.github.com/user")
	if err != nil {
		return "", "", "", fmt.Errorf("github user api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("github user api status: %d", resp.StatusCode)
	}

	var user githubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", "", "", fmt.Errorf("decode github user: %w", err)
	}

	subject := fmt.Sprintf("%d", user.ID)
	displayName := user.Name
	if displayName == "" {
		displayName = user.Login
	}

	// Get verified primary email.
	email := user.Email
	if email == "" {
		email = g.fetchPrimaryEmail(ctx, client)
	}

	return subject, email, displayName, nil
}

func (g *GitHubProvider) fetchPrimaryEmail(ctx context.Context, client *http.Client) string {
	resp, err := client.Get("https://api.github.com/user/emails")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var emails []githubEmail
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return ""
	}

	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email
		}
	}
	// Fall back to first verified email.
	for _, e := range emails {
		if e.Verified {
			return e.Email
		}
	}
	return ""
}

// --- Generic OIDC Provider ---

type GenericOIDCProvider struct {
	name         string
	clientID     string
	clientSecret string
	redirectURL  string
	provider     *gooidc.Provider
	verifier     *gooidc.IDTokenVerifier
}

func NewGenericOIDCProvider(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL string) (*GenericOIDCProvider, error) {
	provider, err := gooidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", issuerURL, err)
	}
	verifier := provider.Verifier(&gooidc.Config{ClientID: clientID})
	return &GenericOIDCProvider{
		name:         "oidc",
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirectURL,
		provider:     provider,
		verifier:     verifier,
	}, nil
}

func (g *GenericOIDCProvider) Name() string { return g.name }

func (g *GenericOIDCProvider) OAuth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     g.clientID,
		ClientSecret: g.clientSecret,
		Endpoint:     g.provider.Endpoint(),
		RedirectURL:  g.redirectURL,
		Scopes:       []string{gooidc.ScopeOpenID, "profile", "email"},
	}
}

func (g *GenericOIDCProvider) GetIdentity(ctx context.Context, token *oauth2.Token) (string, string, string, error) {
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return "", "", "", fmt.Errorf("no id_token in token response")
	}

	idToken, err := g.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", "", "", fmt.Errorf("verify id token: %w", err)
	}

	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", "", "", fmt.Errorf("parse id token claims: %w", err)
	}

	return claims.Sub, claims.Email, claims.Name, nil
}
