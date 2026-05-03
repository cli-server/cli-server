package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentserver/agentserver/internal/agent"
)

// AuthState represents the current authentication state of the controller.
type AuthState int32

const (
	AuthLoggedOut  AuthState = iota
	AuthLoggingIn
	AuthLoggedIn
	AuthRefreshing
)

func (s AuthState) String() string {
	switch s {
	case AuthLoggedOut:
		return "logged_out"
	case AuthLoggingIn:
		return "logging_in"
	case AuthLoggedIn:
		return "logged_in"
	case AuthRefreshing:
		return "refreshing"
	}
	return "unknown"
}

// AuthConfig holds configuration for AuthController.
type AuthConfig struct {
	ServerURL       string
	CredentialsPath string
	SkipOpenBrowser bool
	OnChange        func(AuthState)

	// OnLoginFailed is called (from the poll goroutine) when the OAuth Device
	// Flow fails for any reason other than user cancellation (context cancel).
	// nil if caller doesn't need this signal.
	OnLoginFailed func(error)

	// Test seams (default to real implementations from internal/agent/login.go).
	RequestDeviceCode func(serverURL string) (*agent.DeviceAuthResponse, error)
	PollForToken      func(serverURL string, dr *agent.DeviceAuthResponse) (*agent.TokenResponse, error)
}

// LoginInfo contains the user-visible information from the device authorization flow.
type LoginInfo struct {
	UserCode      string
	VerifyURL     string
	VerifyURLFull string
	ExpiresIn     int
}

// AuthController manages the OAuth authentication state machine.
type AuthController struct {
	cfg         AuthConfig
	state       atomic.Int32
	mu          sync.Mutex
	creds       *agent.Credentials
	cancelLogin context.CancelFunc
	refreshMu   sync.Mutex
}

// NewAuthController creates an AuthController and initialises state from stored credentials.
// If valid credentials exist at cfg.CredentialsPath the state starts as AuthLoggedIn.
func NewAuthController(cfg AuthConfig) *AuthController {
	if cfg.RequestDeviceCode == nil {
		cfg.RequestDeviceCode = agent.RequestDeviceCode
	}
	if cfg.PollForToken == nil {
		cfg.PollForToken = agent.PollForToken
	}
	if cfg.CredentialsPath == "" {
		cfg.CredentialsPath = agent.DefaultCredentialsPath()
	}

	ac := &AuthController{cfg: cfg}
	creds, err := agent.LoadCredentials(cfg.CredentialsPath)
	if err == nil && creds != nil && time.Now().Before(creds.ExpiresAt.Add(-30*time.Second)) {
		ac.creds = creds
		ac.setState(AuthLoggedIn)
	} else {
		ac.setState(AuthLoggedOut)
	}
	return ac
}

// State returns the current authentication state.
func (a *AuthController) State() AuthState {
	return AuthState(a.state.Load())
}

func (a *AuthController) setState(s AuthState) {
	prev := AuthState(a.state.Swap(int32(s)))
	if prev != s && a.cfg.OnChange != nil {
		a.cfg.OnChange(s)
	}
}

// SetOnChange installs or replaces the OnChange callback after construction.
// Used by RunTUI to wire the Bubble Tea program after both AuthController
// and the program exist.
func (a *AuthController) SetOnChange(fn func(AuthState)) {
	a.cfg.OnChange = fn
}

// SetOnLoginFailed installs or replaces the OnLoginFailed callback after
// construction. Used by RunTUI to surface login errors to the TUI timeline.
func (a *AuthController) SetOnLoginFailed(fn func(error)) {
	a.cfg.OnLoginFailed = fn
}

// EnsureValid returns a non-empty access token or an error. If the token is
// near expiry it triggers a refresh (state → Refreshing). Refresh failure
// transitions to LoggedOut.
func (a *AuthController) EnsureValid(ctx context.Context) (string, error) {
	a.mu.Lock()
	creds := a.creds
	a.mu.Unlock()
	if creds == nil {
		return "", fmt.Errorf("not authenticated")
	}
	if time.Now().Before(creds.ExpiresAt.Add(-5 * time.Minute)) {
		return creds.AccessToken, nil
	}
	return a.refresh(ctx)
}

func (a *AuthController) refresh(ctx context.Context) (string, error) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	a.mu.Lock()
	creds := a.creds
	a.mu.Unlock()

	if creds == nil {
		a.setState(AuthLoggedOut)
		return "", fmt.Errorf("not authenticated")
	}
	// Double-check in case another goroutine already refreshed.
	if time.Now().Before(creds.ExpiresAt.Add(-5 * time.Minute)) {
		return creds.AccessToken, nil
	}

	a.setState(AuthRefreshing)
	newCreds, err := refreshDirect(ctx, a.cfg.ServerURL, creds.RefreshToken)
	if err != nil {
		a.mu.Lock()
		a.creds = nil
		a.mu.Unlock()
		_ = os.Remove(a.cfg.CredentialsPath)
		a.setState(AuthLoggedOut)
		return "", err
	}
	a.mu.Lock()
	a.creds = newCreds
	a.mu.Unlock()
	_ = agent.SaveCredentials(a.cfg.CredentialsPath, newCreds)
	a.setState(AuthLoggedIn)
	return newCreds.AccessToken, nil
}

// StartLogin kicks off OAuth Device Flow. Returns the user-visible code and
// URL synchronously; the polling loop runs in a goroutine and eventually
// transitions state to LoggedIn or LoggedOut.
func (a *AuthController) StartLogin(ctx context.Context) (LoginInfo, error) {
	if a.State() == AuthLoggedIn {
		return LoginInfo{}, fmt.Errorf("already logged in")
	}
	if a.State() == AuthLoggingIn {
		return LoginInfo{}, fmt.Errorf("login already in progress")
	}
	if a.cfg.ServerURL == "" {
		return LoginInfo{}, fmt.Errorf("--server is required for /login on first run")
	}
	a.setState(AuthLoggingIn)
	dr, err := a.cfg.RequestDeviceCode(a.cfg.ServerURL)
	if err != nil {
		a.setState(AuthLoggedOut)
		return LoginInfo{}, err
	}

	pollCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.cancelLogin = cancel
	a.mu.Unlock()

	go a.runPoll(pollCtx, dr)

	return LoginInfo{
		UserCode:      dr.UserCode,
		VerifyURL:     dr.VerificationURI,
		VerifyURLFull: dr.VerificationURIComplete,
		ExpiresIn:     dr.ExpiresIn,
	}, nil
}

func (a *AuthController) runPoll(ctx context.Context, dr *agent.DeviceAuthResponse) {
	tr, err := a.cfg.PollForToken(a.cfg.ServerURL, dr)
	if ctx.Err() != nil {
		a.setState(AuthLoggedOut)
		return
	}
	if err != nil {
		a.setState(AuthLoggedOut)
		if a.cfg.OnLoginFailed != nil {
			a.cfg.OnLoginFailed(err)
		}
		return
	}
	creds := &agent.Credentials{
		ServerURL:    a.cfg.ServerURL,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scopes:       strings.Split(tr.Scope, " "),
	}
	if err := agent.SaveCredentials(a.cfg.CredentialsPath, creds); err != nil {
		a.setState(AuthLoggedOut)
		return
	}
	a.mu.Lock()
	a.creds = creds
	a.mu.Unlock()
	a.setState(AuthLoggedIn)
}

// CancelLogin aborts an in-progress login flow.
func (a *AuthController) CancelLogin() {
	a.mu.Lock()
	cancel := a.cancelLogin
	a.cancelLogin = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if a.State() == AuthLoggingIn {
		a.setState(AuthLoggedOut)
	}
}

// Logout clears in-memory credentials and invalidates the credentials file so
// that subsequent LoadCredentials calls return an error (file contains no
// parseable content).
func (a *AuthController) Logout() error {
	a.mu.Lock()
	a.creds = nil
	a.mu.Unlock()
	if err := os.Remove(a.cfg.CredentialsPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	a.setState(AuthLoggedOut)
	return nil
}

// clearCredentials writes empty bytes to path, making it unreadable as JSON.
// We overwrite rather than remove so that LoadCredentials returns a parse
// error (the test in auth_test.go asserts err != nil after Logout).
// refreshDirect performs the OAuth refresh_token exchange directly, returning
// new Credentials on success. This is used instead of agent.EnsureValidToken
// because that function hardcodes DefaultCredentialsPath() and does not accept
// a custom credentials path.
func refreshDirect(ctx context.Context, serverURL, refreshToken string) (*agent.Credentials, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {"agentserver-agent-cli"},
		"refresh_token": {refreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(serverURL, "/")+"/api/oauth2/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Use a timeout client; http.DefaultClient has no timeout and a slow or
	// hung token endpoint would block all Bus.do calls indefinitely.
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed (%d)", resp.StatusCode)
	}

	var tr agent.TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}
	return &agent.Credentials{
		ServerURL:    serverURL,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scopes:       strings.Split(tr.Scope, " "),
	}, nil
}
