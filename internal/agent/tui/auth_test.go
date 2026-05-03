package tui

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/agent"
)

func TestAuth_StartsLoggedOutWhenNoCreds(t *testing.T) {
	ac := NewAuthController(AuthConfig{
		ServerURL:       "https://example",
		CredentialsPath: t.TempDir() + "/creds.json",
	})
	if ac.State() != AuthLoggedOut {
		t.Errorf("state=%v want LoggedOut", ac.State())
	}
}

func TestAuth_StartsLoggedInWhenValidCreds(t *testing.T) {
	p := t.TempDir() + "/creds.json"
	if err := agent.SaveCredentials(p, &agent.Credentials{
		ServerURL:    "https://example",
		AccessToken:  "tk",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	ac := NewAuthController(AuthConfig{
		ServerURL: "https://example", CredentialsPath: p,
	})
	if ac.State() != AuthLoggedIn {
		t.Errorf("state=%v want LoggedIn", ac.State())
	}
	tk, err := ac.EnsureValid(context.Background())
	if err != nil || tk != "tk" {
		t.Errorf("EnsureValid → %q %v want tk nil", tk, err)
	}
}

func TestAuth_LoginFlowSuccess(t *testing.T) {
	var states []AuthState
	var mu sync.Mutex
	ac := NewAuthController(AuthConfig{
		ServerURL:       "https://example",
		CredentialsPath: t.TempDir() + "/creds.json",
		OnChange: func(s AuthState) {
			mu.Lock(); states = append(states, s); mu.Unlock()
		},
		RequestDeviceCode: func(_ string) (*agent.DeviceAuthResponse, error) {
			return &agent.DeviceAuthResponse{
				DeviceCode: "dc", UserCode: "USER-CODE",
				VerificationURI: "https://example/verify",
				ExpiresIn: 60, Interval: 1,
			}, nil
		},
		PollForToken: func(_ string, _ *agent.DeviceAuthResponse) (*agent.TokenResponse, error) {
			return &agent.TokenResponse{AccessToken: "new", RefreshToken: "nr", ExpiresIn: 3600}, nil
		},
	})
	info, err := ac.StartLogin(context.Background())
	if err != nil { t.Fatal(err) }
	if info.UserCode != "USER-CODE" {
		t.Errorf("user_code=%q", info.UserCode)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ac.State() == AuthLoggedIn { break }
		time.Sleep(10 * time.Millisecond)
	}
	if ac.State() != AuthLoggedIn {
		t.Errorf("state after poll = %v want LoggedIn", ac.State())
	}
	mu.Lock()
	if len(states) < 2 || states[0] != AuthLoggingIn {
		t.Errorf("state transitions = %v", states)
	}
	mu.Unlock()
}

func TestAuth_LoginFlowDenied(t *testing.T) {
	ac := NewAuthController(AuthConfig{
		ServerURL:       "https://example",
		CredentialsPath: t.TempDir() + "/creds.json",
		RequestDeviceCode: func(_ string) (*agent.DeviceAuthResponse, error) {
			return &agent.DeviceAuthResponse{DeviceCode: "dc", UserCode: "X", ExpiresIn: 60, Interval: 1}, nil
		},
		PollForToken: func(_ string, _ *agent.DeviceAuthResponse) (*agent.TokenResponse, error) {
			return nil, errors.New("authorization denied by user")
		},
	})
	_, _ = ac.StartLogin(context.Background())
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ac.State() == AuthLoggedOut { break }
		time.Sleep(10 * time.Millisecond)
	}
	if ac.State() != AuthLoggedOut {
		t.Errorf("state after deny = %v want LoggedOut", ac.State())
	}
}

func TestAuth_LoginFlowDenied_CallsOnLoginFailed(t *testing.T) {
	var mu sync.Mutex
	var capturedErr error
	ac := NewAuthController(AuthConfig{
		ServerURL:       "https://example",
		CredentialsPath: t.TempDir() + "/creds.json",
		RequestDeviceCode: func(_ string) (*agent.DeviceAuthResponse, error) {
			return &agent.DeviceAuthResponse{DeviceCode: "dc", UserCode: "X", ExpiresIn: 60, Interval: 1}, nil
		},
		PollForToken: func(_ string, _ *agent.DeviceAuthResponse) (*agent.TokenResponse, error) {
			return nil, errors.New("authorization denied by user")
		},
		OnLoginFailed: func(err error) {
			mu.Lock()
			capturedErr = err
			mu.Unlock()
		},
	})
	_, _ = ac.StartLogin(context.Background())
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := capturedErr
		mu.Unlock()
		if got != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := capturedErr
	mu.Unlock()
	if got == nil {
		t.Error("OnLoginFailed not invoked")
	}
}

func TestAuth_Logout_ClearsCreds(t *testing.T) {
	p := t.TempDir() + "/creds.json"
	_ = agent.SaveCredentials(p, &agent.Credentials{
		ServerURL: "https://example", AccessToken: "tk", ExpiresAt: time.Now().Add(time.Hour),
	})
	ac := NewAuthController(AuthConfig{ServerURL: "https://example", CredentialsPath: p})
	if ac.State() != AuthLoggedIn { t.Fatal("not logged in") }
	if err := ac.Logout(); err != nil { t.Fatal(err) }
	if ac.State() != AuthLoggedOut {
		t.Errorf("state=%v want LoggedOut", ac.State())
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected creds file removed; stat err = %v", err)
	}
}
