package tunnel

// FrameType indicates the kind of tunnel message.
const (
	FrameTypeRequest  = "request"
	FrameTypeResponse = "response"
	FrameTypeStream   = "stream"
)

// RequestFrame is sent from server to agent, representing an HTTP request.
type RequestFrame struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64 encoded
}

// ResponseFrame is sent from agent to server, representing a complete HTTP response.
type ResponseFrame struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64 encoded
}

// StreamFrame is sent from agent to server for streaming (SSE) responses.
type StreamFrame struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Chunk   string            `json:"chunk"`   // base64 encoded
	Done    bool              `json:"done"`
}

// IncomingFrame is used for initial JSON unmarshaling to determine frame type.
type IncomingFrame struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}
