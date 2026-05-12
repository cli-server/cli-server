// Package captoken mints HMAC capability tokens accepted by
// codex-exec-gateway. This is the mint side of the format whose verify
// side lives in internal/codexexecgateway/auth.go. Both sides must stay
// in lockstep — any change to the token format (header constant, payload
// field names, encoding, HMAC algorithm) must be reflected in both
// packages. The cross-service round-trip test in captoken_test.go imports
// codexexecgateway.VerifyCapabilityToken and serves as the contract test.
package captoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
)

// Payload is the cap-token payload shape. Field names and JSON tags are
// intentionally kept identical to codexexecgateway.CapPayload to ensure
// the minted tokens are accepted by the verify side. Do not import
// codexexecgateway.CapPayload directly — that would create a
// cross-service import dependency and prevent independent deployment.
type Payload struct {
	TurnID      string   `json:"turn_id"`
	WorkspaceID string   `json:"workspace_id"`
	ExeIDs      []string `json:"exe_ids"`
	IAT         int64    `json:"iat"`
	EXP         int64    `json:"exp"`
}

// header is the fixed token header as required by the format spec:
//
//	{"alg":"HS256","typ":"CXG"}
//
// This must match the header the verify side expects. Use compact JSON
// (no spaces) to produce a deterministic base64url-encoded value.
var header = mustBase64URL([]byte(`{"alg":"HS256","typ":"CXG"}`))

func mustBase64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// Mint produces a capability token in the format:
//
//	base64url(header) "." base64url(payload) "." base64url(hmac)
//
// The HMAC is SHA-256 keyed by secret over the string
// base64url(header)+"."+base64url(payload), matching what
// codexexecgateway.VerifyCapabilityToken recomputes during verification.
// base64url uses no padding per RFC 7515 / JWT convention.
func Mint(secret []byte, payload Payload) string {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal on a plain struct never errors in practice; panic to
		// surface the programming error early.
		panic("captoken: marshal payload: " + err.Error())
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	signingInput := header + "." + payloadB64

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	sig := mac.Sum(nil)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return signingInput + "." + sigB64
}
