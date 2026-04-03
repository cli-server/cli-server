package sandbox

import (
	"strings"
	"testing"
)

func TestBuildNanoclawConfig_Basic(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy", "", "", "", "", "")
	if !strings.Contains(result, "ANTHROPIC_BASE_URL=https://proxy.example.com") {
		t.Errorf("missing ANTHROPIC_BASE_URL, got: %s", result)
	}
	if !strings.Contains(result, "ANTHROPIC_API_KEY=tok-123") {
		t.Errorf("missing ANTHROPIC_API_KEY, got: %s", result)
	}
	if !strings.Contains(result, "ASSISTANT_NAME=Andy") {
		t.Errorf("missing ASSISTANT_NAME, got: %s", result)
	}
	if !strings.Contains(result, "NANOCLAW_NO_CONTAINER=true") {
		t.Errorf("missing NANOCLAW_NO_CONTAINER, got: %s", result)
	}
	if strings.Contains(result, "NANOCLAW_BRIDGE_URL") {
		t.Errorf("should not contain NANOCLAW_BRIDGE_URL when IM bridge disabled")
	}
	if strings.Contains(result, "NANOCLAW_WEIXIN_BRIDGE_URL") {
		t.Errorf("should not contain NANOCLAW_WEIXIN_BRIDGE_URL when IM bridge disabled")
	}
}

func TestBuildNanoclawConfig_WithWeixin(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy",
		"https://bridge.example.com/weixin", "secret-abc", "", "", "")
	if !strings.Contains(result, "NANOCLAW_BRIDGE_URL=https://bridge.example.com/weixin") {
		t.Errorf("missing NANOCLAW_BRIDGE_URL, got: %s", result)
	}
	if !strings.Contains(result, "NANOCLAW_WEIXIN_BRIDGE_URL=https://bridge.example.com/weixin") {
		t.Errorf("missing NANOCLAW_WEIXIN_BRIDGE_URL (backwards compat), got: %s", result)
	}
	if !strings.Contains(result, "NANOCLAW_BRIDGE_SECRET=secret-abc") {
		t.Errorf("missing NANOCLAW_BRIDGE_SECRET, got: %s", result)
	}
}

func TestBuildNanoclawConfig_BYOK(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy",
		"", "", "https://custom.llm.com", "custom-key-456", "")
	if !strings.Contains(result, "ANTHROPIC_BASE_URL=https://custom.llm.com") {
		t.Errorf("BYOK should override base URL, got: %s", result)
	}
	if !strings.Contains(result, "ANTHROPIC_API_KEY=custom-key-456") {
		t.Errorf("BYOK should override API key, got: %s", result)
	}
}

func TestBuildNanoclawConfig_WithGemini(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy",
		"", "", "", "", "https://gemini-proxy.example.com")
	if !strings.Contains(result, "GOOGLE_GEMINI_BASE_URL=https://gemini-proxy.example.com") {
		t.Errorf("missing GOOGLE_GEMINI_BASE_URL, got: %s", result)
	}
	if !strings.Contains(result, "GEMINI_API_KEY=tok-123") {
		t.Errorf("missing GEMINI_API_KEY, got: %s", result)
	}
}

func TestBuildNanoclawConfig_WithoutGemini(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy",
		"", "", "", "", "")
	if strings.Contains(result, "GOOGLE_GEMINI_BASE_URL") {
		t.Errorf("should not contain GOOGLE_GEMINI_BASE_URL when not configured, got: %s", result)
	}
	if strings.Contains(result, "GEMINI_API_KEY") {
		t.Errorf("should not contain GEMINI_API_KEY when not configured, got: %s", result)
	}
}

func TestBuildNanoclawConfig_GeminiWithBYOK(t *testing.T) {
	result := BuildNanoclawConfig("https://proxy.example.com", "tok-123", "Andy",
		"", "", "https://custom.llm.com", "custom-key-456", "https://gemini-proxy.example.com")
	// BYOK bypasses the proxy — Gemini proxy vars should NOT be injected.
	if strings.Contains(result, "GEMINI_API_KEY") {
		t.Errorf("BYOK should suppress Gemini proxy vars, got: %s", result)
	}
	if strings.Contains(result, "GOOGLE_GEMINI_BASE_URL") {
		t.Errorf("BYOK should suppress Gemini proxy vars, got: %s", result)
	}
}
