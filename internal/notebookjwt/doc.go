// Package notebookjwt mints + verifies short-lived HMAC-SHA256 tokens
// for the agentserver web notebook proxy. Claims: user_id, workspace_id,
// exp. Format mirrors internal/codexappgateway/captoken.go (JWT-shape,
// base64url-no-pad).
package notebookjwt
