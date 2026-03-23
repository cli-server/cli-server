package llmproxy

import "time"

// SandboxInfo is returned by the agentserver token validation API.
type SandboxInfo struct {
	ID                     string `json:"sandbox_id"`
	WorkspaceID            string `json:"workspace_id"`
	Status                 string `json:"status"`
	ModelserverUpstreamURL string `json:"modelserver_upstream_url,omitempty"`
}

// Trace represents a logical session/trace spanning multiple API requests.
type Trace struct {
	ID          string    `json:"id"`
	SandboxID   string    `json:"sandbox_id"`
	WorkspaceID string    `json:"workspace_id"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TokenUsage records a single LLM API request's token usage.
type TokenUsage struct {
	ID                       string    `json:"id"`
	TraceID                  string    `json:"trace_id,omitempty"`
	SandboxID                string    `json:"sandbox_id"`
	WorkspaceID              string    `json:"workspace_id"`
	Provider                 string    `json:"provider"`
	Model                    string    `json:"model"`
	MessageID                string    `json:"message_id,omitempty"`
	InputTokens              int64     `json:"input_tokens"`
	OutputTokens             int64     `json:"output_tokens"`
	CacheCreationInputTokens int64     `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64     `json:"cache_read_input_tokens"`
	Streaming                bool      `json:"streaming"`
	Duration                 int64     `json:"duration"`
	TTFT                     int64     `json:"ttft"`
	CreatedAt                time.Time `json:"created_at"`
}

// UsageSummary is an aggregated usage row grouped by provider+model.
type UsageSummary struct {
	Provider                 string `json:"provider"`
	Model                    string `json:"model"`
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	RequestCount             int64  `json:"request_count"`
}

// TraceWithStats is a trace with aggregated request statistics.
type TraceWithStats struct {
	Trace
	RequestCount             int64  `json:"request_count"`
	TotalInputTokens         int64  `json:"total_input_tokens"`
	TotalOutputTokens        int64  `json:"total_output_tokens"`
	TotalCacheReadTokens     int64  `json:"total_cache_read_tokens"`
	TotalCacheCreationTokens int64  `json:"total_cache_creation_tokens"`
	Models                   string `json:"models"`
}

// QueryOpts filters for usage/trace queries.
type QueryOpts struct {
	WorkspaceID string
	SandboxID   string
	Since       time.Time
	Limit       int
	Offset      int
}

// WorkspaceQuota holds per-workspace quota overrides stored in the llmproxy DB.
type WorkspaceQuota struct {
	WorkspaceID string    `json:"workspace_id"`
	MaxRPD      *int      `json:"max_rpd"`
	UpdatedAt   time.Time `json:"updated_at"`
}
