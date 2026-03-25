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
}
