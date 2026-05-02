package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
)

type sendMessageInput struct {
	Text   string `json:"text"`
	Sender string `json:"sender,omitempty"`
}
type sendImageInput struct {
	// Source is base64-encoded image bytes for now. The HTTP-MCP era
	// resolved URL/executor sources too; that resolver lives in the
	// soon-to-be-deleted mcp_router.go and is not yet ported. Until it
	// is, callers must pass base64 directly.
	Source  string `json:"source"`
	Format  string `json:"format,omitempty"`
	Caption string `json:"caption,omitempty"`
}
type sendFileInput struct {
	Source   string `json:"source"`
	Filename string `json:"filename"`
	Caption  string `json:"caption,omitempty"`
}

func imTools(tctx *Context) []agentsdk.McpTool {
	return []agentsdk.McpTool{
		agentsdk.Tool[sendMessageInput]("send_message",
			"Send a text message to the user in the current IM conversation.",
			func(ctx context.Context, in sendMessageInput) (*agentsdk.McpToolResult, error) {
				if in.Text == "" {
					return errResult(fmt.Errorf("text is required")), nil
				}
				return imbridgePost(tctx, "/api/internal/imbridge/send", map[string]string{
					"channel_id": tctx.IMChannelID,
					"to_user_id": tctx.IMUserID,
					"text":       in.Text,
				})
			}),
		agentsdk.Tool[sendImageInput]("send_image",
			"Send an image to the user in the current IM conversation. `source` must be base64-encoded image bytes.",
			func(ctx context.Context, in sendImageInput) (*agentsdk.McpToolResult, error) {
				if in.Source == "" {
					return errResult(fmt.Errorf("source is required")), nil
				}
				// If the caller already supplied base64, use it directly.
				// Otherwise treat Source as raw bytes and encode.
				b64 := in.Source
				if _, err := base64.StdEncoding.DecodeString(in.Source); err != nil {
					b64 = base64.StdEncoding.EncodeToString([]byte(in.Source))
				}
				body := map[string]string{
					"channel_id":   tctx.IMChannelID,
					"to_user_id":   tctx.IMUserID,
					"image_base64": b64,
				}
				if in.Format != "" {
					body["format"] = in.Format
				}
				if in.Caption != "" {
					body["caption"] = in.Caption
				}
				return imbridgePost(tctx, "/api/internal/imbridge/send-image", body)
			}),
		agentsdk.Tool[sendFileInput]("send_file",
			"Send a file to the user in the current IM conversation.",
			func(ctx context.Context, _ sendFileInput) (*agentsdk.McpToolResult, error) {
				return errResult(fmt.Errorf("send_file is not yet supported by the IM provider")), nil
			}),
	}
}

// imbridgePost executes a POST against the IM bridge service and returns a McpToolResult.
func imbridgePost(tctx *Context, path string, body map[string]string) (*agentsdk.McpToolResult, error) {
	if tctx.IMBridgeURL == "" {
		return errResult(fmt.Errorf("IMBridgeURL is not configured")), nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, tctx.IMBridgeURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tctx.InternalAPISecret != "" {
		req.Header.Set("X-Internal-Secret", tctx.InternalAPISecret)
	}
	resp, err := tctx.HTTP.Do(req)
	if err != nil {
		return errResult(fmt.Errorf("HTTP request failed: %w", err)), nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return errResult(fmt.Errorf("IM bridge returned %d: %s", resp.StatusCode, respBody)), nil
	}
	return &agentsdk.McpToolResult{
		Content: []agentsdk.McpToolContent{{Type: "text", Text: "ok"}},
	}, nil
}
