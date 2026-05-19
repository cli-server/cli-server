package tools

import "testing"

func TestShQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo", "'foo'"},
		{"/path/to/file", "'/path/to/file'"},
		{"with space", "'with space'"},
		{"with'quote", `'with'\''quote'`},
		{"with$dollar", "'with$dollar'"},
		{"`backticks`", "'`backticks`'"},
		{"", "''"},
	}
	for _, c := range cases {
		if got := shQuote(c.in); got != c.want {
			t.Errorf("shQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
