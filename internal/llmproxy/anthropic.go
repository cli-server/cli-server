package llmproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// handleAnthropicProxy proxies Anthropic API requests, recording token usage and trace data.
func (s *Server) handleAnthropicProxy(w http.ResponseWriter, r *http.Request) {
	// 1. Validate proxy token (x-api-key header).
	proxyToken := r.Header.Get("x-api-key")
	if proxyToken == "" {
		http.Error(w, "missing api key", http.StatusUnauthorized)
		return
	}

	sbx, err := s.ValidateProxyToken(r.Context(), proxyToken)
	if err != nil {
		s.logger.Error("token validation failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sbx == nil {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
		return
	}
	if sbx.Status != "running" && sbx.Status != "creating" {
		http.Error(w, "sandbox not active", http.StatusForbidden)
		return
	}

	// 1a. Determine upstream target.
	targetURL := s.config.AnthropicBaseURL
	useModelserver := sbx.ModelserverUpstreamURL != ""
	if useModelserver {
		targetURL = sbx.ModelserverUpstreamURL
	}

	// 1b. Check RPD quota (only for messages endpoint, skip for modelserver).
	isMessagesEndpoint := strings.HasSuffix(r.URL.Path, "/messages")
	if isMessagesEndpoint && !useModelserver {
		if exceeded, current, max := s.checkRPD(sbx.WorkspaceID); exceeded {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(anthropic.ErrorResponse{
				Type: "error",
				Error: anthropic.ErrorObjectUnion{
					Type:    "rate_limit_error",
					Message: fmt.Sprintf("workspace requests per day quota exceeded (%d/%d)", current, max),
				},
			})
			return
		}
	}

	// 2. Read body for trace extraction and stream detection.
	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// Detect streaming from request body.
	var reqShape struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(bodyBytes, &reqShape) // best-effort; ignore errors
	isStreaming := reqShape.Stream

	// 3. Extract trace ID.
	traceID, source := s.ExtractTraceID(r, bodyBytes)
	requestID := GenerateRequestID()

	logger := s.logger.With(
		"trace_id", traceID,
		"request_id", requestID,
		"sandbox_id", sbx.ID,
		"workspace_id", sbx.WorkspaceID,
	)

	// 4. Persist trace (only for messages endpoint).
	if isMessagesEndpoint && s.store != nil {
		if _, err := s.store.GetOrCreateTrace(traceID, sbx.ID, sbx.WorkspaceID, source); err != nil {
			logger.Error("failed to create trace", "error", err)
		}
	}

	// 5. Set up reverse proxy.
	target, err := url.Parse(targetURL)
	if err != nil {
		logger.Error("invalid upstream URL", "error", err)
		http.Error(w, "invalid upstream URL", http.StatusInternalServerError)
		return
	}

	// 5a. Pre-fetch modelserver token (before creating proxy, so we can fail early).
	var msToken string
	if useModelserver {
		var tokenErr error
		msToken, tokenErr = s.fetchModelserverToken(sbx.WorkspaceID)
		if tokenErr != nil {
			logger.Error("failed to get modelserver token", "error", tokenErr)
			http.Error(w, "modelserver token unavailable", http.StatusBadGateway)
			return
		}
	}

	startTime := time.Now()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = r.URL.Path // /v1/* paths map directly
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = target.Host

			if useModelserver {
				// Modelserver auth: Bearer token, no x-api-key.
				req.Header.Del("x-api-key")
				req.Header.Set("Authorization", "Bearer "+msToken)
			} else {
				// Anthropic auth: inject real API credentials.
				if s.config.AnthropicAPIKey != "" {
					req.Header.Set("x-api-key", s.config.AnthropicAPIKey)
				}
				if s.config.AnthropicAuthToken != "" {
					req.Header.Set("Authorization", "Bearer "+s.config.AnthropicAuthToken)
				}
			}
			if req.Header.Get("anthropic-version") == "" {
				req.Header.Set("anthropic-version", "2023-06-01")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if !isMessagesEndpoint {
				return nil
			}
			if isStreaming {
				return s.interceptStreaming(resp, sbx, traceID, requestID, logger, startTime)
			}
			return s.interceptNonStreaming(resp, sbx, traceID, requestID, logger, startTime)
		},
		FlushInterval: -1, // Enable SSE streaming.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("proxy error", "error", err)
			http.Error(w, "proxy error", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}

// interceptNonStreaming reads the full response body, extracts usage, and records it.
func (s *Server) interceptNonStreaming(resp *http.Response, sbx *SandboxInfo, traceID, requestID string, logger *slog.Logger, startTime time.Time) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		logger.Error("failed to read response body", "error", err)
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	model, msgID, usage, err := ParseNonStreamingResponse(body)
	if err != nil {
		logger.Warn("failed to parse response", "error", err)
		return nil
	}

	duration := time.Since(startTime).Milliseconds()
	s.recordUsage(sbx, traceID, requestID, model, msgID, usage, false, duration, 0, logger)
	return nil
}

// interceptStreaming wraps the response body with a stream interceptor.
func (s *Server) interceptStreaming(resp *http.Response, sbx *SandboxInfo, traceID, requestID string, logger *slog.Logger, startTime time.Time) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}

	resp.Body = newStreamInterceptor(resp.Body, startTime, func(model, msgID string, usage anthropic.Usage, ttft int64) {
		duration := time.Since(startTime).Milliseconds()
		s.recordUsage(sbx, traceID, requestID, model, msgID, usage, true, duration, ttft, logger)
	})
	return nil
}

// recordUsage persists a usage record and logs it.
func (s *Server) recordUsage(sbx *SandboxInfo, traceID, requestID, model, msgID string, usage anthropic.Usage, streaming bool, duration, ttft int64, logger *slog.Logger) {
	logger.Info("anthropic request completed",
		"model", model,
		"message_id", msgID,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cache_creation_input_tokens", usage.CacheCreationInputTokens,
		"cache_read_input_tokens", usage.CacheReadInputTokens,
		"streaming", streaming,
		"duration", duration,
		"ttft", ttft,
	)

	if s.store == nil {
		return
	}

	u := TokenUsage{
		ID:                       requestID,
		TraceID:                  traceID,
		SandboxID:                sbx.ID,
		WorkspaceID:              sbx.WorkspaceID,
		Provider:                 "anthropic",
		Model:                    model,
		MessageID:                msgID,
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		Streaming:                streaming,
		Duration:                 duration,
		TTFT:                     ttft,
		CreatedAt:                time.Now(),
	}

	if err := s.store.RecordUsage(u); err != nil {
		logger.Error("failed to record usage", "error", err)
	}
	if err := s.store.UpdateTraceActivity(traceID); err != nil {
		logger.Error("failed to update trace activity", "error", err)
	}
}

// checkRPD resolves the effective max RPD for a workspace and checks if it's exceeded.
// Returns (exceeded, current, max). If max is 0, RPD is unlimited.
func (s *Server) checkRPD(workspaceID string) (bool, int64, int64) {
	maxRPD := s.config.DefaultMaxRPD

	if s.store != nil {
		if wq, err := s.store.GetWorkspaceQuota(workspaceID); err == nil && wq != nil && wq.MaxRPD != nil {
			maxRPD = *wq.MaxRPD
		}
	}

	if maxRPD <= 0 {
		return false, 0, 0
	}

	if s.store == nil {
		return false, 0, int64(maxRPD)
	}

	count, err := s.store.CountTodayRequests(workspaceID)
	if err != nil {
		s.logger.Error("failed to count today requests for RPD check", "error", err, "workspace_id", workspaceID)
		return false, 0, int64(maxRPD)
	}

	if count >= int64(maxRPD) {
		return true, count, int64(maxRPD)
	}
	return false, count, int64(maxRPD)
}
