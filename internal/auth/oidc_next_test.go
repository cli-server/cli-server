package auth

import (
	"os"
	"testing"
)

func TestSafeNext(t *testing.T) {
	t.Setenv("AGENTSERVER_COOKIE_DOMAIN", ".agent.cs.ac.cn")

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/", "/"},
		{"/codex-auth/codex/device", "/codex-auth/codex/device"},
		{"/path?q=1&r=2", "/path?q=1&r=2"},

		{"//evil.com/path", ""}, // protocol-relative
		{"https://evil.com/foo", ""},
		{"http://codex-auth.agent.cs.ac.cn/x", ""}, // wrong scheme
		{"javascript:alert(1)", ""},
		{"https://agent.cs.ac.cn.evil.com/x", ""}, // suffix-attack

		{"https://codex-auth.agent.cs.ac.cn/codex/device", "https://codex-auth.agent.cs.ac.cn/codex/device"},
		{"https://agent.cs.ac.cn/x", "https://agent.cs.ac.cn/x"},

		{"/x\r\nSet-Cookie: a=b", ""}, // CRLF injection
	}
	for _, c := range cases {
		if got := safeNext(c.in); got != c.want {
			t.Errorf("safeNext(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// No cookie domain → absolute URLs rejected even if scheme/host look ok.
	os.Unsetenv("AGENTSERVER_COOKIE_DOMAIN")
	if got := safeNext("https://codex-auth.agent.cs.ac.cn/x"); got != "" {
		t.Errorf("expected absolute URL rejected without cookieDomain, got %q", got)
	}
	if got := safeNext("/relative"); got != "/relative" {
		t.Errorf("relative path should still work without cookieDomain, got %q", got)
	}
}
