package llmproxy

import "testing"

func TestJoinPaths(t *testing.T) {
	cases := []struct {
		base, req, want string
	}{
		{"", "/v1/responses", "/v1/responses"},
		{"/", "/v1/responses", "/v1/responses"},
		{"/api", "/v1/responses", "/api/v1/responses"},
		{"/api/", "/v1/responses", "/api/v1/responses"},
		{"/api", "v1/responses", "/api/v1/responses"},
		{"/api/", "v1/responses", "/api/v1/responses"},
	}
	for _, c := range cases {
		if got := joinPaths(c.base, c.req); got != c.want {
			t.Errorf("joinPaths(%q, %q) = %q, want %q", c.base, c.req, got, c.want)
		}
	}
}
