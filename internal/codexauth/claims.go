package codexauth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// IDTokenClaims is the subset of fields we populate for the OpenAI-shaped
// id_token. Codex never verifies the signature, but it base64-decodes
// the payload and looks for the nested OpenAI claim.
type IDTokenClaims struct {
	Issuer    string
	Subject   string
	Email     string
	ExpiresAt time.Time
}

// BuildIDToken returns a signed RS256 JWT with the OpenAI-nested claim
// that codex's `compose_success_url` and `jwt_auth_claims` paths expect
// (login/src/server.rs:821-907).
func BuildIDToken(key *rsa.PrivateKey, kid string, c IDTokenClaims) (string, error) {
	payload := map[string]any{
		"iss":   c.Issuer,
		"sub":   c.Subject,
		"aud":   "app_EMoamEEZ73f0CkXaXp7hrann",
		"iat":   time.Now().Unix(),
		"exp":   c.ExpiresAt.Unix(),
		"email": c.Email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":            c.Subject,
			"chatgpt_plan_type":             "pro",
			"organization_id":               "org_agentserver",
			"project_id":                    "proj_default",
			"completed_platform_onboarding": true,
			"is_org_owner":                  true,
		},
	}
	return signRS256(key, kid, payload)
}

// BuildAccessToken mints the access_token as an RS256 JWT carrying just
// `{iss, sub, iat, exp}`. Codex never validates the signature (it treats
// access_token as an opaque bearer), but it *does* parse `exp` to drive
// proactive refresh (login/src/auth/manager.rs:1793-1812 —
// is_stale_for_proactive_refresh calls parse_jwt_expiration on
// access_token). Minting the access_token as a JWT means codex refreshes
// cleanly at the real expiry instead of waiting for a 401 retry.
func BuildAccessToken(key *rsa.PrivateKey, kid string, c IDTokenClaims) (string, error) {
	payload := map[string]any{
		"iss": c.Issuer,
		"sub": c.Subject,
		"aud": "app_EMoamEEZ73f0CkXaXp7hrann",
		"iat": time.Now().Unix(),
		"exp": c.ExpiresAt.Unix(),
	}
	return signRS256(key, kid, payload)
}

// AgentIdentityClaims is the JWT codex's exec-server --remote
// --use-agent-identity-auth requires (agent-identity/src/lib.rs:65-78).
type AgentIdentityClaims struct {
	AgentRuntimeID       string
	AgentPrivateKeyPKCS8 []byte
	AccountID            string
	ChatgptUserID        string
	Email                string
	PlanType             string
	ExpiresAt            time.Time
}

// BuildAgentIdentityJWT returns an RS256 JWT whose claims pass codex's
// strict aud="codex-app-server" + iss=".../agent-identity" check.
func BuildAgentIdentityJWT(key *rsa.PrivateKey, kid string, c AgentIdentityClaims) (string, error) {
	payload := map[string]any{
		"iss":                        "https://chatgpt.com/codex-backend/agent-identity",
		"aud":                        "codex-app-server",
		"iat":                        time.Now().Unix(),
		"exp":                        c.ExpiresAt.Unix(),
		"agent_runtime_id":           c.AgentRuntimeID,
		"agent_private_key":          base64.StdEncoding.EncodeToString(c.AgentPrivateKeyPKCS8),
		"account_id":                 c.AccountID,
		"chatgpt_user_id":            c.ChatgptUserID,
		"email":                      c.Email,
		"plan_type":                  c.PlanType,
		"chatgpt_account_is_fedramp": false,
	}
	return signRS256(key, kid, payload)
}

func signRS256(key *rsa.PrivateKey, kid string, payload map[string]any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		return "", fmt.Errorf("new signer: %w", err)
	}
	jws, err := signer.Sign(body)
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	return jws.CompactSerialize()
}
