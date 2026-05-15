package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHMACAuthenticator_RoundTrip(t *testing.T) {
	a := NewHMAC([]byte("secret"))
	tok := a.Mint("ws_alpha", "thr_42")
	if !strings.HasPrefix(tok, "ws_alpha.thr_42.") {
		t.Fatalf("token shape unexpected: %s", tok)
	}
	got, err := a.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.WorkspaceID != "ws_alpha" {
		t.Errorf("decoded = %+v", got)
	}
}

func TestHMACAuthenticator_RejectsBadSig(t *testing.T) {
	a := NewHMAC([]byte("secret"))
	tok := a.Mint("ws_a", "thr_1")
	tampered := tok[:len(tok)-1] + "0"
	if _, err := a.Verify(context.Background(), tampered); err == nil {
		t.Fatal("want signature mismatch error")
	}
}

func TestHMACAuthenticator_RejectsBadShape(t *testing.T) {
	a := NewHMAC([]byte("secret"))
	for _, bad := range []string{"", "no-dots", "one.two", "..."} {
		if _, err := a.Verify(context.Background(), bad); err == nil {
			t.Errorf("want error for %q", bad)
		}
	}
}

func TestExtractBearer(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer foo.bar.baz")
	if got, ok := ExtractBearer(r); !ok || got != "foo.bar.baz" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	r2, _ := http.NewRequest("GET", "/", nil)
	if _, ok := ExtractBearer(r2); ok {
		t.Error("missing header should return false")
	}
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("Authorization", "Basic foo")
	if _, ok := ExtractBearer(r3); ok {
		t.Error("non-Bearer should return false")
	}
}
