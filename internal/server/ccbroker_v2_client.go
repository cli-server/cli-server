package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ccbrokerV2Submit POSTs to cc-broker's /api/v2/turns and returns the
// canonical turn_id assigned by cc-broker (or echoes back the caller-supplied
// one if the body included a "turn_id" field). Body is the same shape as v1:
// {session_id, workspace_id, user_message, im_channel_id?, im_user_id?,
// metadata?, turn_id?}.
//
// Returns an error on any non-202 response. Caller-supplied IDs preserved
// agentserver-side allow the TUI inbound's CAS-on-active_turn flow to refer
// to the same ID cc-broker uses.
func ccbrokerV2Submit(ctx context.Context, brokerURL string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", brokerURL+"/api/v2/turns", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build v2 request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("v2 submit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("v2 submit returned %d: %s", resp.StatusCode, respBody)
	}
	var out struct {
		TurnID string `json:"turn_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode v2 response: %w", err)
	}
	if out.TurnID == "" {
		return "", fmt.Errorf("v2 response missing turn_id")
	}
	return out.TurnID, nil
}

// ccbrokerOpenEventStream opens GET /api/turns/{tid}/events and returns the
// raw response body for SSE parsing. Caller is responsible for closing the
// returned body.
//
// The stream wire format is identical to v1's POST /api/turns response, so
// existing parsers (extractFinalText, the bufio.Scanner loop in
// handler_tui_inbound) work unchanged.
func ccbrokerOpenEventStream(ctx context.Context, brokerURL, turnID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", brokerURL+"/api/turns/"+turnID+"/events", nil)
	if err != nil {
		return nil, fmt.Errorf("build events request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open events stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("events stream returned %d: %s", resp.StatusCode, respBody)
	}
	return resp.Body, nil
}
