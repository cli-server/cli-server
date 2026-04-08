package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HydraClient talks to the Ory Hydra Admin API.
type HydraClient struct {
	AdminURL  string // e.g. "http://hydra:4445"
	PublicURL string // e.g. "https://auth.example.com"
	client    *http.Client
}

// NewHydraClient creates a client for the given Hydra Admin URL.
func NewHydraClient(adminURL, publicURL string) *HydraClient {
	return &HydraClient{
		AdminURL:  strings.TrimRight(adminURL, "/"),
		PublicURL: strings.TrimRight(publicURL, "/"),
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

// --- Types ---

type LoginRequest struct {
	Challenge      string   `json:"challenge"`
	Subject        string   `json:"subject"`
	Skip           bool     `json:"skip"`
	RequestedScope []string `json:"requested_scope"`
	Client         struct {
		ClientID string `json:"client_id"`
	} `json:"client"`
}

type AcceptLoginBody struct {
	Subject     string `json:"subject"`
	Remember    bool   `json:"remember"`
	RememberFor int    `json:"remember_for,omitempty"`
}

type ConsentRequest struct {
	Challenge      string   `json:"challenge"`
	Subject        string   `json:"subject"`
	RequestedScope []string `json:"requested_scope"`
	Client         struct {
		ClientID string `json:"client_id"`
	} `json:"client"`
}

type ConsentSession struct {
	AccessToken map[string]interface{} `json:"access_token,omitempty"`
	IDToken     map[string]interface{} `json:"id_token,omitempty"`
}

type AcceptConsentBody struct {
	GrantScope  []string       `json:"grant_scope"`
	Session     ConsentSession `json:"session"`
	Remember    bool           `json:"remember,omitempty"`
	RememberFor int            `json:"remember_for,omitempty"`
}

type RejectBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

type RedirectResponse struct {
	RedirectTo string `json:"redirect_to"`
}

type IntrospectionResult struct {
	Active   bool                   `json:"active"`
	Subject  string                 `json:"sub"`
	Scope    string                 `json:"scope"`
	ClientID string                 `json:"client_id"`
	Extra    map[string]interface{} `json:"ext"`
}

// --- Login Provider API ---

func (h *HydraClient) GetLoginRequest(challenge string) (*LoginRequest, error) {
	u := h.AdminURL + "/admin/oauth2/auth/requests/login?login_challenge=" + url.QueryEscape(challenge)
	resp, err := h.client.Get(u)
	if err != nil {
		return nil, fmt.Errorf("get login request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get login request: status %d: %s", resp.StatusCode, body)
	}
	var req LoginRequest
	if err := json.NewDecoder(resp.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode login request: %w", err)
	}
	return &req, nil
}

func (h *HydraClient) AcceptLogin(challenge string, body AcceptLoginBody) (string, error) {
	return h.putJSON("/admin/oauth2/auth/requests/login/accept", "login_challenge", challenge, body)
}

func (h *HydraClient) RejectLogin(challenge string, body RejectBody) (string, error) {
	return h.putJSON("/admin/oauth2/auth/requests/login/reject", "login_challenge", challenge, body)
}

// --- Consent Provider API ---

func (h *HydraClient) GetConsentRequest(challenge string) (*ConsentRequest, error) {
	u := h.AdminURL + "/admin/oauth2/auth/requests/consent?consent_challenge=" + url.QueryEscape(challenge)
	resp, err := h.client.Get(u)
	if err != nil {
		return nil, fmt.Errorf("get consent request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get consent request: status %d: %s", resp.StatusCode, body)
	}
	var req ConsentRequest
	if err := json.NewDecoder(resp.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode consent request: %w", err)
	}
	return &req, nil
}

func (h *HydraClient) AcceptConsent(challenge string, body AcceptConsentBody) (string, error) {
	return h.putJSON("/admin/oauth2/auth/requests/consent/accept", "consent_challenge", challenge, body)
}

func (h *HydraClient) RejectConsent(challenge string, body RejectBody) (string, error) {
	return h.putJSON("/admin/oauth2/auth/requests/consent/reject", "consent_challenge", challenge, body)
}

// --- Device Flow ---

type AcceptDeviceBody struct {
	UserCode string `json:"user_code"`
}

func (h *HydraClient) AcceptDeviceChallenge(challenge string, body AcceptDeviceBody) (string, error) {
	return h.putJSON("/admin/oauth2/auth/requests/device/accept", "device_challenge", challenge, body)
}

// --- Token Introspection ---

func (h *HydraClient) IntrospectToken(token string) (*IntrospectionResult, error) {
	form := url.Values{"token": {token}}
	resp, err := h.client.PostForm(h.AdminURL+"/admin/oauth2/introspect", form)
	if err != nil {
		return nil, fmt.Errorf("introspect token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("introspect token: status %d: %s", resp.StatusCode, body)
	}
	var result IntrospectionResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode introspection: %w", err)
	}
	return &result, nil
}

// HasScope checks if the introspection result includes the given scope.
func (r *IntrospectionResult) HasScope(scope string) bool {
	for _, s := range strings.Split(r.Scope, " ") {
		if s == scope {
			return true
		}
	}
	return false
}

// --- Helpers ---

func (h *HydraClient) putJSON(path, queryKey, queryVal string, body interface{}) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal body: %w", err)
	}
	u := h.AdminURL + path + "?" + queryKey + "=" + url.QueryEscape(queryVal)
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("put request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("put %s: status %d: %s", path, resp.StatusCode, respBody)
	}
	var rr RedirectResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return "", fmt.Errorf("decode redirect: %w", err)
	}
	return rr.RedirectTo, nil
}
