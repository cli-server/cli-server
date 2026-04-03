package llmproxy

import (
	"net/http"

	"github.com/google/uuid"
)

// Trace ID prefixes.
const (
	traceIDPrefix   = "at-"
	requestIDPrefix = "ar-"

	geminiTraceIDPrefix   = "gt-"
	geminiRequestIDPrefix = "gr-"
)

// ExtractTraceID extracts a trace ID from the request.
// Priority: custom header → OpenCode session header → auto-generate.
// Returns (traceID, source).
func (s *Server) ExtractTraceID(r *http.Request, body []byte) (string, string) {
	// 1. Check custom trace header.
	if s.config.TraceHeader != "" {
		if hdr := r.Header.Get(s.config.TraceHeader); hdr != "" {
			return hdr, "header"
		}
	}

	// 2. Try OpenCode x-opencode-session header.
	if hdr := r.Header.Get("x-opencode-session"); hdr != "" {
		return traceIDPrefix + hdr, "opencode"
	}

	// 3. Auto-generate.
	return GenerateTraceID(), "auto"
}

// GenerateTraceID creates a new trace ID with the "at-" prefix.
func GenerateTraceID() string {
	return traceIDPrefix + uuid.New().String()
}

// GenerateRequestID creates a new request ID with the "ar-" prefix.
func GenerateRequestID() string {
	return requestIDPrefix + uuid.New().String()
}

// GenerateGeminiTraceID creates a new trace ID with the "gt-" prefix.
func GenerateGeminiTraceID() string {
	return geminiTraceIDPrefix + uuid.New().String()
}

// GenerateGeminiRequestID creates a new request ID with the "gr-" prefix.
func GenerateGeminiRequestID() string {
	return geminiRequestIDPrefix + uuid.New().String()
}

// ExtractGeminiTraceID extracts a trace ID from the request for Gemini.
// Same priority as ExtractTraceID but uses gt- prefix for auto-generated IDs.
func (s *Server) ExtractGeminiTraceID(r *http.Request, body []byte) (string, string) {
	// 1. Check custom trace header.
	if s.config.TraceHeader != "" {
		if hdr := r.Header.Get(s.config.TraceHeader); hdr != "" {
			return hdr, "header"
		}
	}

	// 2. Try OpenCode x-opencode-session header.
	if hdr := r.Header.Get("x-opencode-session"); hdr != "" {
		return geminiTraceIDPrefix + hdr, "opencode"
	}

	// 3. Auto-generate.
	return GenerateGeminiTraceID(), "auto"
}
