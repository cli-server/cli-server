package notebookjwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Claims are what callers get back from Verify.
type Claims struct {
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	Exp         int64  `json:"exp"`
}

const header = `{"alg":"HS256","typ":"AS-NOTEBOOK"}`

// Mint produces a token valid for ttl from now.
func Mint(secret []byte, userID, workspaceID string, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", fmt.Errorf("notebookjwt: empty secret")
	}
	if userID == "" || workspaceID == "" {
		return "", fmt.Errorf("notebookjwt: user_id/workspace_id required")
	}
	c := Claims{
		UserID:      userID,
		WorkspaceID: workspaceID,
		Exp:         time.Now().Add(ttl).Unix(),
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("notebookjwt: marshal: %w", err)
	}
	enc := base64.RawURLEncoding
	headerB64 := enc.EncodeToString([]byte(header))
	bodyB64 := enc.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(headerB64 + "." + bodyB64))
	return headerB64 + "." + bodyB64 + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// Verify parses, checks signature + expiry, returns claims.
func Verify(secret []byte, tok string) (*Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("notebookjwt: malformed token")
	}
	enc := base64.RawURLEncoding
	wantSig, err := enc.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("notebookjwt: sig decode: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), wantSig) {
		return nil, fmt.Errorf("notebookjwt: signature mismatch")
	}
	bodyBytes, err := enc.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("notebookjwt: body decode: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(bodyBytes, &c); err != nil {
		return nil, fmt.Errorf("notebookjwt: body parse: %w", err)
	}
	if c.Exp < time.Now().Unix() {
		return nil, fmt.Errorf("notebookjwt: token expired (exp=%d, now=%d)", c.Exp, time.Now().Unix())
	}
	if c.UserID == "" || c.WorkspaceID == "" {
		return nil, fmt.Errorf("notebookjwt: missing required claims")
	}
	return &c, nil
}
