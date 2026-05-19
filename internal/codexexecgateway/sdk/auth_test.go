package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyTokenAuth_VerifySuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Secret") != "test-secret" {
			t.Errorf("missing X-Internal-Secret header")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"workspace_id": "ws-1", "user_id": "u-1",
		})
	}))
	defer upstream.Close()
	a := NewProxyTokenAuth(upstream.URL, "test-secret", time.Minute, time.Second)
	wsID, userID, err := a.Verify(context.Background(), "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if wsID != "ws-1" || userID != "u-1" {
		t.Errorf("got wsID=%q userID=%q", wsID, userID)
	}
}

func TestProxyTokenAuth_CacheHit(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]string{"workspace_id": "ws-1", "user_id": "u-1"})
	}))
	defer upstream.Close()
	a := NewProxyTokenAuth(upstream.URL, "test-secret", time.Minute, time.Second)
	for i := 0; i < 5; i++ {
		if _, _, err := a.Verify(context.Background(), "tok-1"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 upstream call (cache should serve rest), got %d", calls)
	}
}

func TestProxyTokenAuth_VerifyUnauthorized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	a := NewProxyTokenAuth(upstream.URL, "test-secret", time.Minute, time.Second)
	if _, _, err := a.Verify(context.Background(), "tok-bad"); err == nil {
		t.Fatal("expected error")
	}
}

func TestProxyTokenAuth_NegativeCache(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "invalid", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	a := NewProxyTokenAuth(upstream.URL, "test-secret", time.Minute, time.Second)
	for i := 0; i < 3; i++ {
		if _, _, err := a.Verify(context.Background(), "tok-bad"); err == nil {
			t.Fatal("expected error")
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 upstream call (negative cache should serve rest), got %d", calls)
	}
}
