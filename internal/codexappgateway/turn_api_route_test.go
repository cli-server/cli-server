package codexappgateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTurnAPIRequiresInternalSecret(t *testing.T) {
	s := &Server{cfg: ServeConfig{AgentserverInternalSecret: "s3cret"}}
	mw := s.requireInternalSecret(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("no header", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/turns", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("code=%d want 401", w.Code)
		}
	})
	t.Run("wrong header", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/turns", strings.NewReader("{}"))
		r.Header.Set("X-Internal-Secret", "nope")
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("code=%d", w.Code)
		}
	})
	t.Run("correct", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/turns", strings.NewReader("{}"))
		r.Header.Set("X-Internal-Secret", "s3cret")
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Errorf("code=%d", w.Code)
		}
	})
}
