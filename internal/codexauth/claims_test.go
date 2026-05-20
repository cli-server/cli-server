package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildIDToken_CarriesOpenAIClaim(t *testing.T) {
	kid, kp, _ := GenerateRSAKey()
	issuer := "https://example/codex-auth"
	jwt, err := BuildIDToken(kp.PrivateKey, kid, IDTokenClaims{
		Issuer:    issuer,
		Subject:   "user-abc",
		Email:     "u@test",
		ExpiresAt: time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("BuildIDToken: %v", err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not 3-part JWT: %d parts", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The nested OpenAI claim is critical — codex parses it.
	oai, ok := claims["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		t.Fatalf("missing https://api.openai.com/auth claim: %v", claims)
	}
	if oai["chatgpt_account_id"] != "user-abc" {
		t.Errorf("chatgpt_account_id = %v", oai["chatgpt_account_id"])
	}
	if oai["chatgpt_plan_type"] != "pro" {
		t.Errorf("chatgpt_plan_type = %v", oai["chatgpt_plan_type"])
	}
}

func TestBuildAccessToken_HasExpClaim(t *testing.T) {
	kid, kp, _ := GenerateRSAKey()
	exp := time.Now().Add(1 * time.Hour)
	jwt, err := BuildAccessToken(kp.PrivateKey, kid, IDTokenClaims{
		Issuer:    "https://example/codex-auth",
		Subject:   "user-abc",
		ExpiresAt: exp,
	})
	if err != nil {
		t.Fatalf("BuildAccessToken: %v", err)
	}
	parts := strings.Split(jwt, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(payload, &claims)
	// Codex's parse_jwt_expiration reads `exp` and triggers refresh
	// when exp <= now — so we must mint this value.
	if claims["exp"] == nil {
		t.Errorf("exp claim missing: %v", claims)
	}
	if claims["sub"] != "user-abc" {
		t.Errorf("sub = %v", claims["sub"])
	}
}

func TestBuildAgentIdentityJWT_IsCodexParseable(t *testing.T) {
	kid, kp, _ := GenerateRSAKey()
	_, pub, _ := GenerateEd25519Key()
	priv, _, _ := GenerateEd25519Key()
	jwt, err := BuildAgentIdentityJWT(kp.PrivateKey, kid, AgentIdentityClaims{
		AgentRuntimeID:       "exe_xyz",
		AgentPrivateKeyPKCS8: priv,
		AccountID:            "u-1",
		ChatgptUserID:        "u-1",
		Email:                "u@test",
		PlanType:             "pro",
		ExpiresAt:            time.Now().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("BuildAgentIdentityJWT: %v", err)
	}
	parts := strings.Split(jwt, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(payload, &claims)
	// Codex enforces these literals (agent-identity/src/lib.rs:163-166).
	if claims["iss"] != "https://chatgpt.com/codex-backend/agent-identity" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if claims["aud"] != "codex-app-server" {
		t.Errorf("aud = %v", claims["aud"])
	}
	// agent_private_key is base64(PKCS#8 DER) of an Ed25519 key.
	apk, _ := claims["agent_private_key"].(string)
	if apk == "" {
		t.Error("agent_private_key missing")
	}
	_ = pub // referenced for future tests; not asserted here
}
