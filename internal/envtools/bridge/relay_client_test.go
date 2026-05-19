package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRelayClient_Disabled(t *testing.T) {
	c := NewRelayClient("", "", "ws", nil)
	if c.Enabled() {
		t.Error("Enabled() = true on empty config")
	}
	_, err := c.CreateRelay(context.Background(), "a", "b", time.Minute, 0)
	if !errors.Is(err, ErrRelayDisabled) {
		t.Errorf("err = %v, want ErrRelayDisabled", err)
	}
}

func TestRelayClient_CreateRelay_Success(t *testing.T) {
	var gotBody relayCreateBody
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/exec-gateway/relay/create" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(RelayTicket{
			Ticket:      "rly_abc",
			UploadURL:   "https://relay.example/relay/rly_abc",
			DownloadURL: "https://relay.example/relay/rly_abc",
			ExpiresAt:   time.Now().Add(5 * time.Minute),
		})
	}))
	defer ts.Close()

	c := NewRelayClient(ts.URL, "test-secret", "ws_1", nil)
	if !c.Enabled() {
		t.Fatal("Enabled() = false; want true")
	}
	tk, err := c.CreateRelay(context.Background(), "src_exe", "dst_exe", 60*time.Second, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Ticket != "rly_abc" {
		t.Errorf("ticket = %q", tk.Ticket)
	}
	if gotAuth != "Bearer test-secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody.WorkspaceID != "ws_1" || gotBody.SourceExeID != "src_exe" ||
		gotBody.DestExeID != "dst_exe" || gotBody.TTLSeconds != 60 || gotBody.MaxBytes != 1024 {
		t.Errorf("request body wrong: %+v", gotBody)
	}
}

func TestRelayClient_CreateRelay_ErrorPropagates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not your executor"}`))
	}))
	defer ts.Close()
	c := NewRelayClient(ts.URL, "s", "w", nil)
	_, err := c.CreateRelay(context.Background(), "a", "b", time.Minute, 0)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want one mentioning 403", err)
	}
}
