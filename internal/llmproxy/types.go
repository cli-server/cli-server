package llmproxy

import "time"

// SandboxInfo is returned by the agentserver token validation API.
type SandboxInfo struct {
	ID          string `json:"sandbox_id"`
	WorkspaceID string `json:"workspace_id"`
	Status      string `json:"status"`
}

// Trace represents a logical session/trace spanning multiple API requests.
type Trace struct {
	ID          string    `json:"id"`
	SandboxID   string    `json:"sandboxId"`
	WorkspaceID string    `json:"workspaceId"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// TokenUsage records a single LLM API request's token usage.
type TokenUsage struct {
	ID                       string    `json:"id"`
	TraceID                  string    `json:"traceId,omitempty"`
	SandboxID                string    `json:"sandboxId"`
	WorkspaceID              string    `json:"workspaceId"`
	Provider                 string    `json:"provider"`
	Model                    string    `json:"model"`
	MessageID                string    `json:"messageId,omitempty"`
	InputTokens              int64     `json:"inputTokens"`
	OutputTokens             int64     `json:"outputTokens"`
	CacheCreationInputTokens int64     `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int64     `json:"cacheReadInputTokens"`
	Streaming                bool      `json:"streaming"`
	DurationMs               int64     `json:"durationMs"`
	TTFTMs                   int64     `json:"ttftMs"`
	CreatedAt                time.Time `json:"createdAt"`
}

// UsageSummary is an aggregated usage row grouped by provider+model.
type UsageSummary struct {
	Provider                 string `json:"provider"`
	Model                    string `json:"model"`
	InputTokens              int64  `json:"inputTokens"`
	OutputTokens             int64  `json:"outputTokens"`
	CacheCreationInputTokens int64  `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int64  `json:"cacheReadInputTokens"`
	RequestCount             int64  `json:"requestCount"`
}

// TraceWithStats is a trace with aggregated request statistics.
type TraceWithStats struct {
	Trace
	RequestCount    int64 `json:"requestCount"`
	TotalInputTokens  int64 `json:"totalInputTokens"`
	TotalOutputTokens int64 `json:"totalOutputTokens"`
}

// QueryOpts filters for usage/trace queries.
type QueryOpts struct {
	WorkspaceID string
	SandboxID   string
	Since       time.Time
	Limit       int
}
