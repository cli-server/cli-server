package clientmeta

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP_XForwardedForWins(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Fatalf("want 203.0.113.7, got %q", got)
	}
}

func TestClientIP_XRealIPFallback(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Real-IP", "203.0.113.8")
	if got := ClientIP(r); got != "203.0.113.8" {
		t.Fatalf("want 203.0.113.8, got %q", got)
	}
}

func TestClientIP_RemoteAddrStripsPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	if got := ClientIP(r); got != "10.0.0.1" {
		t.Fatalf("want 10.0.0.1, got %q", got)
	}
}

func TestParseCodexUA(t *testing.T) {
	cases := []struct {
		ua          string
		wantVersion string
		wantOS      string
	}{
		{"codex_cli_rs/0.130.0 (Darwin 24.4.0; arm64)", "0.130.0", "Darwin"},
		{"codex_cli_rs/0.128.0 (Linux 6.5.0-15-generic; x86_64)", "0.128.0", "Linux"},
		{"codex/0.131.1 (Windows NT 10.0; x64)", "0.131.1", "Windows"},
		{"curl/7.88.1", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.ua, func(t *testing.T) {
			v, o := ParseCodexUA(tc.ua)
			if v != tc.wantVersion {
				t.Errorf("version: got %q want %q", v, tc.wantVersion)
			}
			if o != tc.wantOS {
				t.Errorf("os: got %q want %q", o, tc.wantOS)
			}
		})
	}
}

// Sanity: ClientIP with a totally empty request never panics.
func TestClientIP_EmptyRequest(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = ""
	if got := ClientIP(r); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}
