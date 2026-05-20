package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/codexauth"

	"github.com/go-chi/chi/v5"
)

// executorNameRe is the allowed name shape: alphanumeric + dot, dash,
// underscore, 1–64 chars. Names go directly into a shell single-quoted
// argument in the connect command we surface to users (see line ~95);
// restricting the charset blocks any escape attempt without needing
// shell-aware quoting.
var executorNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type registerExecutorReq struct {
	// Name is workspace-unique, surfaced to the LLM (via env-mcp's
	// list_environments) and used as the env_id parameter on shell /
	// apply_patch / etc. Required since v0.54.0.
	Name string `json:"name"`
	// Description is an optional per-binding note shown alongside
	// Name in list_environments. Free-text, no uniqueness constraint.
	Description string `json:"description,omitempty"`
}

type registerExecutorResp struct {
	ExeID             string `json:"exe_id"`
	RegistrationToken string `json:"registration_token"`
	// ConnectCommand is the one-liner the user pastes on the machine
	// they want to expose as an executor. Empty when the gateway public
	// host isn't configured — the UI falls back to a generic template.
	// Kept for backward compatibility with UI clients predating the
	// 3-variant bundle (set to the Agent Identity command when codexAuth
	// is enabled so old clients still get a working command).
	ConnectCommand string `json:"connect_command,omitempty"`
	// AgentIdentityJWT is the codex Agent Identity JWT minted alongside
	// the bcrypt registration token. Present only when codexAuth is
	// enabled (CODEX_AUTH_ISSUER_URL set).
	AgentIdentityJWT string `json:"agent_identity_jwt,omitempty"`
	// ConnectCommands is the 3-variant bundle surfaced by the Add
	// Connector UI. Empty when codexAuth is disabled.
	ConnectCommands ConnectCommands `json:"connect_commands,omitempty"`
}

// ConnectCommands is the 3-variant connect-command bundle returned by
// the Add Connector API. Agent Identity is the recommended path for
// unattended machines; ChatGPT browser is the recommended path for
// developer laptops; ChatGPT device-auth covers the headless +
// ChatGPT-account case.
type ConnectCommands struct {
	AgentIdentity     string `json:"agent_identity"`
	ChatgptBrowser    string `json:"chatgpt_browser"`
	ChatgptDeviceAuth string `json:"chatgpt_device_auth"`
}

// handleRegisterExecutor mints a new executor owned by the calling user
// and immediately binds it to the workspace. ACL: caller must be
// owner/maintainer of the workspace.
//
// Returns the raw registration_token ONCE — UI must show it immediately
// and let the user copy. agentserver does not store the raw token.
func (s *Server) handleRegisterExecutor(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wid := chi.URLParam(r, "wid")
	if wid == "" {
		http.Error(w, "workspace id required", http.StatusBadRequest)
		return
	}
	if !s.requireWorkspaceRole(w, r, wid, "owner", "maintainer") {
		return
	}

	var req registerExecutorReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if !executorNameRe.MatchString(req.Name) {
		http.Error(w, "name must be 1-64 chars of [A-Za-z0-9._-]", http.StatusBadRequest)
		return
	}
	if s.ExecutorsClient == nil {
		http.Error(w, "executors integration not configured", http.StatusServiceUnavailable)
		return
	}

	reg, err := s.ExecutorsClient.Register(r.Context(), userID, RegisterExecutorRequest{
		DisplayName: req.Name, // reuse for the system-level display_name; binding name is the one the LLM sees
	})
	if err != nil {
		http.Error(w, "register: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Auto-bind to the workspace this request was issued under, with
	// the user-supplied name and description. On bind failure (e.g.
	// duplicate name), tear down the freshly-registered executor so we
	// don't leak an orphan row with a wasted exe_id + registration
	// token. The cleanup is best-effort — if it also fails we log
	// (via the bridge error message) but the partial state will at
	// worst occupy a UUID + a never-bound bcrypt hash.
	if err := s.ExecutorsClient.Bind(r.Context(), userID, wid, reg.ExeID, req.Name, req.Description, false); err != nil {
		if cleanupErr := s.ExecutorsClient.Unregister(r.Context(), userID, reg.ExeID); cleanupErr != nil {
			http.Error(w, fmt.Sprintf("bind failed (%v); cleanup of orphan executor also failed (%v)", err, cleanupErr), http.StatusBadGateway)
			return
		}
		http.Error(w, "bind: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Mint Agent Identity JWT alongside the legacy bearer registration.
	// The exe_id is the agent_runtime_id (1-to-1 mapping). Best-effort
	// email lookup — empty email is OK, JWT just won't carry it.
	var aiResult *codexauth.MintAgentIdentityResult
	if s.CodexAuth != nil {
		var email string
		if user, uerr := s.Auth.GetUserByID(userID); uerr == nil && user != nil {
			email = user.Email
		}
		var mintErr error
		aiResult, mintErr = s.CodexAuth.MintAgentIdentity(r.Context(),
			codexauth.MintAgentIdentityArgs{
				AgentRuntimeID: reg.ExeID,
				UserID:         userID,
				Email:          email,
			})
		if mintErr != nil {
			http.Error(w, "mint agent identity: "+mintErr.Error(), http.StatusInternalServerError)
			return
		}
	}

	resp := registerExecutorResp{
		ExeID:             reg.ExeID,
		RegistrationToken: reg.RegistrationToken,
	}
	if aiResult != nil {
		resp.AgentIdentityJWT = aiResult.JWT
	}
	if s.CodexExecGatewayPublicHost != "" {
		// Upstream codex `exec-server --remote` contract:
		//   1. POST <base_url>/cloud/executor/{id}/register with Bearer <token>
		//   2. Server returns {executor_id, url}, codex then ws-dials url.
		// Surface the user's `name` as codex's --name (visible in their
		// shell + tracing) so it lines up with what the LLM sees.
		gatewayURL := "https://" + s.CodexExecGatewayPublicHost
		issuer := s.CodexAuthIssuerURL
		if issuer == "" {
			// codexAuth disabled — legacy bearer-only command (no Agent
			// Identity, no ChatGPT login). Preserves pre-D3 behaviour.
			resp.ConnectCommand = fmt.Sprintf(
				"export CODEX_EXEC_SERVER_REMOTE_BEARER_TOKEN='%s'\ncodex exec-server --remote '%s' --executor-id '%s' --name '%s'",
				reg.RegistrationToken, gatewayURL, reg.ExeID, req.Name)
		} else {
			// codexAuth enabled — surface all 3 variants. Keep the legacy
			// single-string field populated with the Agent Identity
			// command so unrevised UI clients still get a working paste.
			resp.ConnectCommands = ConnectCommands{
				AgentIdentity: fmt.Sprintf(
					"export CODEX_ACCESS_TOKEN='%s'\nexport CODEX_AGENT_IDENTITY_AUTHAPI_BASE_URL='%s'\ncodex -c chatgpt.base_url='%s' exec-server --remote '%s' --executor-id '%s' --name '%s' --use-agent-identity-auth",
					aiResult.JWT, issuer, issuer, gatewayURL, reg.ExeID, req.Name),
				ChatgptBrowser: fmt.Sprintf(
					"codex login --issuer %s\nexport CODEX_REFRESH_TOKEN_URL_OVERRIDE='%s/oauth/token'\ncodex exec-server --remote '%s' --executor-id '%s' --name '%s'",
					issuer, issuer, gatewayURL, reg.ExeID, req.Name),
				ChatgptDeviceAuth: fmt.Sprintf(
					"codex login --device-auth --issuer %s\nexport CODEX_REFRESH_TOKEN_URL_OVERRIDE='%s/oauth/token'\ncodex exec-server --remote '%s' --executor-id '%s' --name '%s'",
					issuer, issuer, gatewayURL, reg.ExeID, req.Name),
			}
			resp.ConnectCommand = resp.ConnectCommands.AgentIdentity
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleListExecutors returns executors bound to the workspace.
// ACL: any workspace member.
func (s *Server) handleListExecutors(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wid := chi.URLParam(r, "wid")
	if _, ok := s.requireWorkspaceMember(w, r, wid); !ok {
		return
	}
	if s.ExecutorsClient == nil {
		_ = json.NewEncoder(w).Encode([]ListedExecutor{})
		return
	}

	rows, err := s.ExecutorsClient.List(r.Context(), userID, wid)
	if err != nil {
		http.Error(w, "list: "+err.Error(), http.StatusBadGateway)
		return
	}
	if rows == nil {
		rows = []ListedExecutor{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

// handleUnbindExecutor removes an executor from the workspace. ACL:
// owner/maintainer of the workspace.
func (s *Server) handleUnbindExecutor(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wid := chi.URLParam(r, "wid")
	exeID := chi.URLParam(r, "exe_id")
	if wid == "" || exeID == "" {
		http.Error(w, "wid and exe_id required", http.StatusBadRequest)
		return
	}
	if !s.requireWorkspaceRole(w, r, wid, "owner", "maintainer") {
		return
	}
	if s.ExecutorsClient == nil {
		http.Error(w, "executors integration not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.ExecutorsClient.Unbind(r.Context(), userID, wid, exeID); err != nil {
		http.Error(w, "unbind: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

