package weixin

import (
	"encoding/json"
	"testing"
)

func TestGetUpdatesRequestJSON(t *testing.T) {
	req := GetUpdatesRequest{
		GetUpdatesBuf: "abc123",
		BaseInfo:      BaseInfo{ChannelVersion: channelVersion},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if s == "" {
		t.Fatal("empty json")
	}
	// Verify round-trip
	var decoded GetUpdatesRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.GetUpdatesBuf != "abc123" {
		t.Errorf("got buf=%q, want abc123", decoded.GetUpdatesBuf)
	}
}

func TestSendMessageRequestJSON(t *testing.T) {
	req := SendMessageRequest{
		Msg: WeixinMessage{
			ToUserID:     "user@im.wechat",
			ClientID:     "test-1",
			MessageType:  2,
			MessageState: 2,
			ContextToken: "ctx-tok",
			ItemList: []MessageItem{{
				Type:     1,
				TextItem: &TextItem{Text: "hello"},
			}},
		},
		BaseInfo: BaseInfo{ChannelVersion: channelVersion},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded SendMessageRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Msg.ToUserID != "user@im.wechat" {
		t.Errorf("got to=%q", decoded.Msg.ToUserID)
	}
	if len(decoded.Msg.ItemList) != 1 || decoded.Msg.ItemList[0].TextItem.Text != "hello" {
		t.Error("text item mismatch")
	}
	if decoded.Msg.ContextToken != "ctx-tok" {
		t.Errorf("got context_token=%q", decoded.Msg.ContextToken)
	}
}

func TestBuildILinkHeaders(t *testing.T) {
	h := buildILinkHeaders("tok-abc")
	if h.Get("Authorization") != "Bearer tok-abc" {
		t.Errorf("got auth=%q", h.Get("Authorization"))
	}
	if h.Get("AuthorizationType") != "ilink_bot_token" {
		t.Errorf("got authtype=%q", h.Get("AuthorizationType"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Errorf("got ct=%q", h.Get("Content-Type"))
	}
	if h.Get("iLink-App-Id") != iLinkAppID {
		t.Errorf("got iLink-App-Id=%q, want %q", h.Get("iLink-App-Id"), iLinkAppID)
	}
	if h.Get("iLink-App-ClientVersion") != iLinkAppClientVersion {
		t.Errorf("got iLink-App-ClientVersion=%q, want %q", h.Get("iLink-App-ClientVersion"), iLinkAppClientVersion)
	}
	if h.Get("X-WECHAT-UIN") == "" {
		t.Error("X-WECHAT-UIN must be present")
	}
}

func TestBuildILinkHeadersNoToken(t *testing.T) {
	h := buildILinkHeaders("")
	if h.Get("Authorization") != "" {
		t.Errorf("expected no Authorization when token empty, got %q", h.Get("Authorization"))
	}
	if h.Get("iLink-App-Id") != iLinkAppID {
		t.Errorf("App-Id must be set even without token, got %q", h.Get("iLink-App-Id"))
	}
}

func TestParseILinkURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		// Allowed: weixin.qq.com family.
		{"default api host", "https://ilinkai.weixin.qq.com", false},
		{"default cdn host", "https://novac2c.cdn.weixin.qq.com/c2c", false},
		{"weixin root exact", "https://weixin.qq.com", false},
		{"weixin deep subdomain", "https://a.b.c.weixin.qq.com/x", false},
		{"weixin host with port", "https://ilinkai.weixin.qq.com:443/path", false},
		{"weixin host case-insensitive", "https://ILINKAI.WEIXIN.QQ.COM", false},
		// Allowed: wechat.com family (international brand).
		{"wechat root", "https://wechat.com", false},
		{"wechat subdomain", "https://api.wechat.com/foo", false},
		// Rejected: scheme.
		{"http rejected", "http://ilinkai.weixin.qq.com", true},
		{"file rejected", "file:///etc/passwd", true},
		{"javascript rejected", "javascript:alert(1)", true},
		// Rejected: host.
		{"different domain", "https://evil.com", true},
		{"weixin-look-alike suffix attack", "https://attacker.weixin.qq.com.evil.com", true},
		{"wechat-look-alike suffix attack", "https://wechat.com.evil.com", true},
		{"empty host", "https:///path", true},
		{"localhost rejected", "https://localhost", true},
		{"raw ip rejected", "https://127.0.0.1", true},
		// Malformed.
		{"unparseable", "://broken", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseILinkURL(tt.url)
			if tt.wantErr && err == nil {
				t.Errorf("expected error for %q, got nil", tt.url)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tt.url, err)
			}
		})
	}
}
