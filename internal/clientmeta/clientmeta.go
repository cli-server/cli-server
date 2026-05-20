// Package clientmeta extracts originating-client metadata (IP, codex
// version, OS) from inbound HTTP requests. Used by both codex-exec-gateway
// (executors connecting from user laptops) and codex-app-gateway (codex
// --remote CLI sessions) so the Browsers + Connectors UI columns derive
// from identical logic.
package clientmeta

import (
	"net"
	"net/http"
	"regexp"
	"strings"
)

// ClientIP returns the best-effort originating client IP for an inbound
// HTTP request. The kube ingress chain prepends entries to
// X-Forwarded-For ("client, proxy1, proxy2"); the first non-empty hop is
// the real client. Falls back to X-Real-IP, then r.RemoteAddr (host:port)
// with the port stripped.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, hop := range strings.Split(xff, ",") {
			hop = strings.TrimSpace(hop)
			if hop != "" {
				return hop
			}
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	if r.RemoteAddr != "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			return host
		}
		return r.RemoteAddr
	}
	return ""
}

// ParseCodexUA extracts the codex CLI version and host OS from the
// User-Agent string of a codex client (exec-server inbound or `codex
// --remote` ws). Upstream codex's reqwest client sends UAs shaped like:
//
//	codex_cli_rs/0.130.0 (Darwin 24.4.0; arm64)
//	codex_cli_rs/0.128.0 (Linux 6.5.0-15-generic; x86_64)
//
// We pattern-match conservatively and return ("", "") on anything we
// don't recognise so callers can store NULL and the UI shows "—".
func ParseCodexUA(ua string) (version, osStr string) {
	if ua == "" {
		return "", ""
	}
	if m := codexUAVersionRE.FindStringSubmatch(ua); m != nil {
		version = m[1]
	}
	if m := codexUAOSRE.FindStringSubmatch(ua); m != nil {
		osStr = m[1]
	}
	return version, osStr
}

var (
	codexUAVersionRE = regexp.MustCompile(`(?i)\bcodex(?:_cli_rs|_cli|)\s*/\s*([0-9][\w.\-+]*)`)
	codexUAOSRE      = regexp.MustCompile(`\(\s*([A-Za-z][A-Za-z0-9_-]*)`)
)
