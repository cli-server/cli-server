package codexappgateway

import "testing"

func TestParseInitializeClientInfo(t *testing.T) {
	cases := []struct {
		name        string
		frame       string
		wantOK      bool
		wantUA      string
		wantVersion string
	}{
		{
			name:        "codex cli initialize",
			frame:       `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"codex_cli_rs","version":"0.132.0"}}}`,
			wantOK:      true,
			wantUA:      "codex_cli_rs/0.132.0",
			wantVersion: "0.132.0",
		},
		{
			name:        "vscode initialize with title",
			frame:       `{"method":"initialize","params":{"clientInfo":{"name":"codex_vscode","title":"VS Code","version":"0.1.0"},"capabilities":{}}}`,
			wantOK:      true,
			wantUA:      "codex_vscode/0.1.0",
			wantVersion: "0.1.0",
		},
		{
			name:   "non-initialize method",
			frame:  `{"method":"thread/start","params":{}}`,
			wantOK: false,
		},
		{
			name:   "no clientInfo",
			frame:  `{"method":"initialize","params":{}}`,
			wantOK: false,
		},
		{
			name:   "invalid JSON",
			frame:  `not json`,
			wantOK: false,
		},
		{
			name:   "binary noise",
			frame:  "\x00\x01\x02",
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ua, version, osStr, ok := parseInitializeClientInfo([]byte(c.frame))
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (ua=%q version=%q os=%q)", ok, c.wantOK, ua, version, osStr)
			}
			if !ok {
				return
			}
			if ua != c.wantUA {
				t.Errorf("ua = %q, want %q", ua, c.wantUA)
			}
			if version != c.wantVersion {
				t.Errorf("version = %q, want %q", version, c.wantVersion)
			}
			if osStr != "" {
				t.Errorf("os = %q, want empty (not in initialize protocol)", osStr)
			}
		})
	}
}
