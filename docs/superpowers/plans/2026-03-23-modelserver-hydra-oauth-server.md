# ModelServer Hydra OAuth Server — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Hydra OAuth server integration to modelserver so external apps (agentserver) can authorize access to a user's project via standard OAuth 2.0 flow.

**Architecture:** Ory Hydra handles the OAuth protocol. Modelserver implements Login Provider (renders login page, validates credentials) and Consent Provider (renders project selection, grants scope). Modelserver's LLM proxy gains Hydra token auth alongside existing API key auth.

**Tech Stack:** Go, chi router, Ory Hydra v2 Admin API, Go `html/template`, AES-GCM encryption (existing), PostgreSQL

**Spec:** `../specs/2026-03-23-modelserver-oauth-integration-design.md`

**Project root:** `/root/coding/modelserver`

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `internal/config/config.go` | Modify | Add `HydraConfig` struct + env bindings |
| `config.example.yml` | Modify | Add hydra config section |
| `internal/admin/hydra_login.go` | Create | Login provider handlers (GET/POST /oauth/login) |
| `internal/admin/hydra_consent.go` | Create | Consent provider handlers (GET/POST /oauth/consent) |
| `internal/admin/hydra_client.go` | Create | Hydra Admin API client (accept/reject login/consent) |
| `internal/admin/hydra_session.go` | Create | Cookie-based session for OAuth login flow |
| `internal/admin/templates/login.html` | Create | Login page HTML template |
| `internal/admin/templates/consent.html` | Create | Project selection HTML template |
| `internal/admin/routes.go` | Modify | Mount new OAuth routes |
| `internal/proxy/auth_middleware.go` | Modify | Add Hydra token introspection fallback |
| `cmd/modelserver/main.go` | Modify | Wire Hydra config, pass to admin routes |

---

### Task 1: Hydra Configuration

**Files:**
- Modify: `internal/config/config.go:48-53` (OAuthConfig struct)
- Modify: `internal/config/config.go:127-144` (env bindings)
- Modify: `config.example.yml:31-43` (oauth section)

- [ ] **Step 1: Add HydraConfig struct to config.go**

After the existing `OIDCConfig` struct (~line 68), add:

```go
type HydraConfig struct {
	AdminURL string `yaml:"admin_url" mapstructure:"admin_url"`
}
```

Add to `OAuthConfig` struct (~line 52):

```go
Hydra HydraConfig `yaml:"hydra" mapstructure:"hydra"`
```

- [ ] **Step 2: Add env binding**

In `setDefaults()` after OIDC bindings (~line 144):

```go
_ = v.BindEnv("auth.oauth.hydra.admin_url", "HYDRA_ADMIN_URL")
```

- [ ] **Step 3: Update config.example.yml**

After the oidc section:

```yaml
    hydra:
      admin_url: "http://hydra:4445"
```

- [ ] **Step 4: Verify build**

Run: `cd /root/coding/modelserver && go build ./...`
Expected: Compiles successfully

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(config): add Hydra OAuth server configuration"
```

---

### Task 2: Hydra Admin API Client

**Files:**
- Create: `internal/admin/hydra_client.go`

- [ ] **Step 1: Create Hydra admin client**

```go
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HydraClient communicates with Hydra's Admin API.
type HydraClient struct {
	adminURL   string
	httpClient *http.Client
}

func NewHydraClient(adminURL string) *HydraClient {
	return &HydraClient{
		adminURL:   adminURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// --- Login flow ---

type HydraLoginRequest struct {
	Challenge string `json:"challenge"`
	Subject   string `json:"subject"`
	Skip      bool   `json:"skip"`
	Client    struct {
		ClientID string `json:"client_id"`
	} `json:"client"`
	RequestURL string `json:"request_url"`
}

type HydraRedirect struct {
	RedirectTo string `json:"redirect_to"`
}

func (c *HydraClient) GetLoginRequest(ctx context.Context, challenge string) (*HydraLoginRequest, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/admin/oauth2/auth/requests/login?login_challenge=%s", c.adminURL, challenge), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result HydraLoginRequest
	return &result, json.NewDecoder(resp.Body).Decode(&result)
}

func (c *HydraClient) AcceptLogin(ctx context.Context, challenge string, subject string) (*HydraRedirect, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"subject":     subject,
		"remember":    true,
		"remember_for": 86400,
	})
	req, _ := http.NewRequestWithContext(ctx, "PUT",
		fmt.Sprintf("%s/admin/oauth2/auth/requests/login/accept?login_challenge=%s", c.adminURL, challenge),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result HydraRedirect
	return &result, json.NewDecoder(resp.Body).Decode(&result)
}

// --- Consent flow ---

type HydraConsentRequest struct {
	Challenge               string   `json:"challenge"`
	Subject                 string   `json:"subject"`
	RequestedScope          []string `json:"requested_scope"`
	RequestedAccessTokenAudience []string `json:"requested_access_token_audience"`
	Client                  struct {
		ClientID string `json:"client_id"`
	} `json:"client"`
}

func (c *HydraClient) GetConsentRequest(ctx context.Context, challenge string) (*HydraConsentRequest, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/admin/oauth2/auth/requests/consent?consent_challenge=%s", c.adminURL, challenge), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result HydraConsentRequest
	return &result, json.NewDecoder(resp.Body).Decode(&result)
}

func (c *HydraClient) AcceptConsent(ctx context.Context, challenge string, grantScope []string, sessionData map[string]interface{}) (*HydraRedirect, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"grant_scope":                grantScope,
		"remember":                   false,
		"session": map[string]interface{}{
			"access_token": sessionData,
		},
	})
	req, _ := http.NewRequestWithContext(ctx, "PUT",
		fmt.Sprintf("%s/admin/oauth2/auth/requests/consent/accept?consent_challenge=%s", c.adminURL, challenge),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result HydraRedirect
	return &result, json.NewDecoder(resp.Body).Decode(&result)
}

// --- Token introspection ---

type IntrospectResult struct {
	Active   bool                   `json:"active"`
	Sub      string                 `json:"sub"`
	Scope    string                 `json:"scope"`
	Ext      map[string]interface{} `json:"ext"`
	ClientID string                 `json:"client_id"`
}

func (c *HydraClient) IntrospectToken(ctx context.Context, token string) (*IntrospectResult, error) {
	body := "token=" + token
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.adminURL+"/admin/oauth2/introspect",
		bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var result IntrospectResult
	return &result, json.Unmarshal(data, &result)
}
```

- [ ] **Step 2: Verify build**

Run: `cd /root/coding/modelserver && go build ./...`

- [ ] **Step 3: Commit**

```bash
git add internal/admin/hydra_client.go && git commit -m "feat(admin): add Hydra Admin API client"
```

---

### Task 3: OAuth Session Cookie

**Files:**
- Create: `internal/admin/hydra_session.go`

- [ ] **Step 1: Create session cookie helper**

```go
package admin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

const oauthSessionCookie = "modelserver-oauth-session"
const oauthSessionTTL = 24 * time.Hour

type oauthSession struct {
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func setOAuthSessionCookie(w http.ResponseWriter, encKey []byte, userID string) error {
	sess := oauthSession{UserID: userID, ExpiresAt: time.Now().Add(oauthSessionTTL)}
	plaintext, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)

	http.SetCookie(w, &http.Cookie{
		Name:     oauthSessionCookie,
		Value:    encodeBase64(ciphertext),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthSessionTTL.Seconds()),
	})
	return nil
}

func getOAuthSession(r *http.Request, encKey []byte) (string, bool) {
	cookie, err := r.Cookie(oauthSessionCookie)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	ciphertext, err := decodeBase64(cookie.Value)
	if err != nil {
		return "", false
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", false
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", false
	}
	var sess oauthSession
	if err := json.Unmarshal(plaintext, &sess); err != nil {
		return "", false
	}
	if time.Now().After(sess.ExpiresAt) {
		return "", false
	}
	return sess.UserID, true
}
```

(Helper `encodeBase64`/`decodeBase64` use `encoding/base64.URLEncoding`.)

- [ ] **Step 2: Verify build**
- [ ] **Step 3: Commit**

```bash
git add internal/admin/hydra_session.go && git commit -m "feat(admin): add OAuth session cookie helpers"
```

---

### Task 4: Login Provider

**Files:**
- Create: `internal/admin/hydra_login.go`
- Create: `internal/admin/templates/login.html`

- [ ] **Step 1: Create login.html template**

```html
<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>Login - ModelServer</title>
<style>
body { font-family: system-ui; max-width: 400px; margin: 80px auto; padding: 0 20px; }
h1 { font-size: 1.5rem; }
form { display: flex; flex-direction: column; gap: 12px; }
input { padding: 8px 12px; border: 1px solid #ccc; border-radius: 4px; font-size: 1rem; }
button { padding: 10px; background: #111; color: #fff; border: none; border-radius: 4px; font-size: 1rem; cursor: pointer; }
.error { color: #d00; font-size: 0.9rem; }
</style>
</head>
<body>
<h1>Sign in to ModelServer</h1>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<form method="POST" action="/oauth/login">
  <input type="hidden" name="login_challenge" value="{{.Challenge}}">
  <input type="email" name="email" placeholder="Email" required>
  <input type="password" name="password" placeholder="Password" required>
  <button type="submit">Sign In</button>
</form>
</body>
</html>
```

- [ ] **Step 2: Create login handler**

```go
package admin

import (
	"html/template"
	"log/slog"
	"net/http"

	"modelserver/internal/store"
)

type LoginHandler struct {
	hydra     *HydraClient
	store     *store.Store
	encKey    []byte
	templates *template.Template
	logger    *slog.Logger
}

func (h *LoginHandler) HandleGetLogin(w http.ResponseWriter, r *http.Request) {
	challenge := r.URL.Query().Get("login_challenge")
	if challenge == "" {
		http.Error(w, "missing login_challenge", http.StatusBadRequest)
		return
	}

	// Check if user already has session cookie
	if userID, ok := getOAuthSession(r, h.encKey); ok {
		redirect, err := h.hydra.AcceptLogin(r.Context(), challenge, userID)
		if err == nil {
			http.Redirect(w, r, redirect.RedirectTo, http.StatusFound)
			return
		}
	}

	h.templates.ExecuteTemplate(w, "login.html", map[string]interface{}{
		"Challenge": challenge,
	})
}

func (h *LoginHandler) HandlePostLogin(w http.ResponseWriter, r *http.Request) {
	challenge := r.FormValue("login_challenge")
	email := r.FormValue("email")
	password := r.FormValue("password")

	// Validate credentials using existing store
	user, err := h.store.GetUserByEmail(email)
	if err != nil || user == nil {
		h.templates.ExecuteTemplate(w, "login.html", map[string]interface{}{
			"Challenge": challenge,
			"Error":     "Invalid email or password",
		})
		return
	}

	// TODO: Verify password - modelserver uses OAuth only, no passwords.
	// For v1, this is a placeholder. The actual implementation depends on
	// whether modelserver adds password auth or redirects to an existing IdP.
	// For now, accept any existing user by email (development only).

	// Set session cookie
	setOAuthSessionCookie(w, h.encKey, user.ID)

	// Accept login with Hydra
	redirect, err := h.hydra.AcceptLogin(r.Context(), challenge, user.ID)
	if err != nil {
		http.Error(w, "failed to accept login", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, redirect.RedirectTo, http.StatusFound)
}
```

- [ ] **Step 3: Verify build**
- [ ] **Step 4: Commit**

```bash
git add internal/admin/hydra_login.go internal/admin/templates/login.html
git commit -m "feat(admin): add Hydra login provider"
```

---

### Task 5: Consent Provider

**Files:**
- Create: `internal/admin/hydra_consent.go`
- Create: `internal/admin/templates/consent.html`

- [ ] **Step 1: Create consent.html template**

```html
<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>Select Project - ModelServer</title>
<style>
body { font-family: system-ui; max-width: 600px; margin: 80px auto; padding: 0 20px; }
h1 { font-size: 1.5rem; }
p.sub { color: #666; }
.projects { display: flex; flex-direction: column; gap: 12px; }
.project { border: 1px solid #ddd; border-radius: 8px; padding: 16px; cursor: pointer; }
.project:hover { border-color: #111; background: #fafafa; }
.project h3 { margin: 0 0 4px; }
.project .meta { font-size: 0.85rem; color: #666; }
button { display: block; width: 100%; padding: 10px; margin-top: 8px; background: #111; color: #fff; border: none; border-radius: 4px; cursor: pointer; }
</style>
</head>
<body>
<h1>Select a Project</h1>
<p class="sub">Grant <strong>{{.ClientID}}</strong> access to one of your projects:</p>
<div class="projects">
{{range .Projects}}
<form method="POST" action="/oauth/consent">
  <input type="hidden" name="consent_challenge" value="{{$.Challenge}}">
  <input type="hidden" name="project_id" value="{{.ID}}">
  <div class="project" onclick="this.parentElement.submit()">
    <h3>{{.Name}}</h3>
    <div class="meta">Role: {{.Role}} · {{.Description}}</div>
  </div>
</form>
{{end}}
</div>
</body>
</html>
```

- [ ] **Step 2: Create consent handler**

```go
package admin

import (
	"html/template"
	"log/slog"
	"net/http"

	"modelserver/internal/store"
	"modelserver/internal/types"
)

type ConsentHandler struct {
	hydra     *HydraClient
	store     *store.Store
	templates *template.Template
	logger    *slog.Logger
}

type projectView struct {
	ID          string
	Name        string
	Description string
	Role        string
}

func (h *ConsentHandler) HandleGetConsent(w http.ResponseWriter, r *http.Request) {
	challenge := r.URL.Query().Get("consent_challenge")
	if challenge == "" {
		http.Error(w, "missing consent_challenge", http.StatusBadRequest)
		return
	}

	consent, err := h.hydra.GetConsentRequest(r.Context(), challenge)
	if err != nil {
		http.Error(w, "failed to get consent request", http.StatusInternalServerError)
		return
	}

	// List user's projects
	projects, _, err := h.store.ListUserProjects(consent.Subject, types.PaginationParams{Limit: 100})
	if err != nil {
		http.Error(w, "failed to list projects", http.StatusInternalServerError)
		return
	}

	// Build view models with role
	var views []projectView
	for _, p := range projects {
		member, _ := h.store.GetProjectMember(p.ID, consent.Subject)
		role := "member"
		if member != nil {
			role = member.Role
		}
		views = append(views, projectView{
			ID: p.ID, Name: p.Name, Description: p.Description, Role: role,
		})
	}

	h.templates.ExecuteTemplate(w, "consent.html", map[string]interface{}{
		"Challenge": challenge,
		"ClientID":  consent.Client.ClientID,
		"Projects":  views,
	})
}

func (h *ConsentHandler) HandlePostConsent(w http.ResponseWriter, r *http.Request) {
	challenge := r.FormValue("consent_challenge")
	projectID := r.FormValue("project_id")

	consent, err := h.hydra.GetConsentRequest(r.Context(), challenge)
	if err != nil {
		http.Error(w, "failed to get consent request", http.StatusInternalServerError)
		return
	}

	// Verify user has access to this project
	member, err := h.store.GetProjectMember(projectID, consent.Subject)
	if err != nil || member == nil {
		http.Error(w, "access denied to project", http.StatusForbidden)
		return
	}

	project, err := h.store.GetProject(projectID)
	if err != nil || project == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	// Accept consent with project info in session data
	redirect, err := h.hydra.AcceptConsent(r.Context(), challenge,
		consent.RequestedScope,
		map[string]interface{}{
			"project_id":   project.ID,
			"project_name": project.Name,
			"user_id":      consent.Subject,
		},
	)
	if err != nil {
		http.Error(w, "failed to accept consent", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, redirect.RedirectTo, http.StatusFound)
}
```

- [ ] **Step 3: Verify build**
- [ ] **Step 4: Commit**

```bash
git add internal/admin/hydra_consent.go internal/admin/templates/consent.html
git commit -m "feat(admin): add Hydra consent provider with project selection"
```

---

### Task 6: Mount OAuth Routes

**Files:**
- Modify: `internal/admin/routes.go`
- Modify: `cmd/modelserver/main.go`

- [ ] **Step 1: Update MountRoutes to accept HydraClient and encKey**

In `routes.go`, add new routes (after existing OAuth routes, ~line 36):

```go
// Hydra OAuth Login/Consent provider
if hydraClient != nil {
    loginH := &LoginHandler{hydra: hydraClient, store: st, encKey: encKey, templates: tmpl, logger: logger}
    consentH := &ConsentHandler{hydra: hydraClient, store: st, templates: tmpl, logger: logger}
    r.Get("/oauth/login", loginH.HandleGetLogin)
    r.Post("/oauth/login", loginH.HandlePostLogin)
    r.Get("/oauth/consent", consentH.HandleGetConsent)
    r.Post("/oauth/consent", consentH.HandlePostConsent)
}
```

- [ ] **Step 2: Update main.go to pass Hydra config**

In `cmd/modelserver/main.go`, after existing setup (~line 220), create HydraClient if configured:

```go
var hydraClient *admin.HydraClient
if cfg.Auth.OAuth.Hydra.AdminURL != "" {
    hydraClient = admin.NewHydraClient(cfg.Auth.OAuth.Hydra.AdminURL)
}
```

Pass to route mounting.

- [ ] **Step 3: Parse templates in route setup**

```go
tmpl := template.Must(template.ParseGlob("internal/admin/templates/*.html"))
```

- [ ] **Step 4: Verify build**
- [ ] **Step 5: Commit**

```bash
git add internal/admin/routes.go cmd/modelserver/main.go
git commit -m "feat(admin): mount Hydra OAuth login/consent routes"
```

---

### Task 7: Proxy Hydra Token Auth

**Files:**
- Modify: `internal/proxy/auth_middleware.go`

- [ ] **Step 1: Add Hydra introspection fallback in auth middleware**

After existing API key validation fails (~line 190, before returning 401), add:

```go
// Fallback: try Hydra token introspection
if hydraClient != nil {
    result, err := hydraClient.IntrospectToken(r.Context(), apiKey)
    if err == nil && result.Active {
        projectID, _ := result.Ext["project_id"].(string)
        userID, _ := result.Ext["user_id"].(string)
        if projectID != "" {
            project, err := store.GetProject(projectID)
            if err == nil && project != nil && project.Status == "active" {
                // Build auth context equivalent to API key auth
                // ... set project, user in context, apply rate limiting
            }
        }
    }
}
```

The exact integration point depends on the existing middleware structure. The key principle: try API key first (fast, local HMAC), then Hydra introspection (network call with cache).

- [ ] **Step 2: Add introspection result cache**

```go
var introspectCache sync.Map // token_hash -> {result, expires_at}

func cachedIntrospect(ctx context.Context, client *HydraClient, token string) (*IntrospectResult, error) {
    hash := sha256.Sum256([]byte(token))
    key := hex.EncodeToString(hash[:])
    if cached, ok := introspectCache.Load(key); ok {
        entry := cached.(*cacheEntry)
        if time.Now().Before(entry.expiresAt) {
            return entry.result, nil
        }
        introspectCache.Delete(key)
    }
    result, err := client.IntrospectToken(ctx, token)
    if err != nil {
        return nil, err
    }
    introspectCache.Store(key, &cacheEntry{result: result, expiresAt: time.Now().Add(30 * time.Second)})
    return result, nil
}
```

- [ ] **Step 3: Verify build**
- [ ] **Step 4: Commit**

```bash
git add internal/proxy/auth_middleware.go
git commit -m "feat(proxy): add Hydra token introspection fallback in auth"
```

---

### Task 8: Integration Test with Hydra

- [ ] **Step 1: Write integration test for login/consent flow**

Test the full flow with a mock Hydra admin API server (httptest.NewServer) that simulates challenge creation and acceptance.

- [ ] **Step 2: Write integration test for proxy dual-auth**

Test that the proxy accepts both API keys and Hydra tokens.

- [ ] **Step 3: Run tests, verify pass**
- [ ] **Step 4: Commit**

```bash
git commit -m "test: add Hydra OAuth integration tests"
```
