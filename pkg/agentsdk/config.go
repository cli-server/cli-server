package agentsdk

import (
	"context"
	"net/http"
	"time"
)

// Config configures the agent client.
type Config struct {
	ServerURL string // Base URL of agentserver (e.g. "https://agent.example.com")
	Name      string // Display name. Defaults to hostname.
	Type      string // Sandbox type. Defaults to "custom".
}

// Registration holds tokens returned after agent registration.
type Registration struct {
	SandboxID   string `json:"sandbox_id"`
	TunnelToken string `json:"tunnel_token"`
	ProxyToken  string `json:"proxy_token"`
	WorkspaceID string `json:"workspace_id"`
	ShortID     string `json:"short_id"`
}

// Handlers defines callbacks for handling requests from agentserver.
type Handlers struct {
	HTTP         http.Handler // Proxied HTTP requests (optional)
	Task         TaskHandler  // Assigned tasks (optional)
	OnConnect    func()       // Called when tunnel connected
	OnDisconnect func(error)  // Called when tunnel disconnected
}

// TaskHandler processes an assigned task. The context is cancelled when the
// tunnel connection is lost or the agent is shutting down.
type TaskHandler func(ctx context.Context, task *Task) error

// Task represents an assigned task.
type Task struct {
	ID             string `json:"task_id"`
	Skill          string `json:"skill"`
	Prompt         string `json:"prompt"`
	SystemContext  string `json:"system_context"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	proxyToken     string
	serverURL      string
}

// TaskResult is the result of a completed task.
type TaskResult struct {
	Output   string  `json:"output"`
	CostUSD  float64 `json:"cost_usd,omitempty"`
	NumTurns int     `json:"num_turns,omitempty"`
}

// DeviceAuthResponse from RequestDeviceCode.
type DeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenResponse from PollForToken.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// ConnectOption configures the Connect call.
type ConnectOption func(*connectOptions)

type connectOptions struct {
	heartbeatInterval time.Duration
	taskPollInterval  time.Duration
}

// WithHeartbeatInterval sets the interval between heartbeat control messages.
// Default is 20 seconds.
func WithHeartbeatInterval(d time.Duration) ConnectOption {
	return func(o *connectOptions) { o.heartbeatInterval = d }
}

// WithTaskPollInterval sets the interval between task poll requests.
// Default is 5 seconds.
func WithTaskPollInterval(d time.Duration) ConnectOption {
	return func(o *connectOptions) { o.taskPollInterval = d }
}
