package auth

import (
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
	got, err := a.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.WorkspaceID != "ws_alpha" || got.ThreadID != "thr_42" {
		t.Errorf("decoded = %+v", got)
	}
}

func TestHMACAuthenticator_RejectsBadSig(t *testing.T) {
	a := NewHMAC([]byte("secret"))
	tok := a.Mint("ws_a", "thr_1")
	tampered := tok[:len(tok)-1] + "0"
	if _, err := a.Verify(tampered); err == nil {
		t.Fatal("want signature mismatch error")
	}
}

func TestHMACAuthenticator_RejectsBadShape(t *testing.T) {
	a := NewHMAC([]byte("secret"))
	for _, bad := range []string{"", "no-dots", "one.two", "..."} {
		if _, err := a.Verify(bad); err == nil {
			t.Errorf("want error for %q", bad)
		}
	}
}

func TestHMACAuthenticator_RoundTrip_DottedIDs(t *testing.T) {
	// workspaceID has no dots (first field); threadID may contain dots
	// (everything between first and last dot in the token prefix).
	a := NewHMAC([]byte("secret"))
	tok := a.Mint("ws_alpha", "thr.42.extra")
	got, err := a.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.WorkspaceID != "ws_alpha" {
		t.Errorf("WorkspaceID = %q", got.WorkspaceID)
	}
	if got.ThreadID != "thr.42.extra" {
		t.Errorf("ThreadID = %q", got.ThreadID)
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
