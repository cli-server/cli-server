package codexauth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
)

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func base64URLNoPad(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// absoluteRequestURL reconstructs the full https://host/path?query URL
// from a request. Needed when handlers redirect cross-subdomain — the
// browser must resolve next= back to this exact codex-auth URL after
// the main-app login completes. RequestURI alone is relative and
// would resolve against the main-app host instead.
//
// Trusts X-Forwarded-Proto when set (Gateway / Ingress in front);
// defaults to https since codex requires TLS.
func absoluteRequestURL(r *http.Request) string {
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	return scheme + "://" + host + r.URL.RequestURI()
}
