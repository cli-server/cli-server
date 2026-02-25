package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	cookieName = "cli-server-token"
	tokenTTL   = 7 * 24 * time.Hour
)

type Auth struct {
	password string
	tokens   map[string]time.Time
	mu       sync.RWMutex
}

func New(password string) *Auth {
	return &Auth{
		password: password,
		tokens:   make(map[string]time.Time),
	}
}

func (a *Auth) Login(password string) (string, bool) {
	if password != a.password {
		return "", false
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	a.mu.Lock()
	a.tokens[token] = time.Now().Add(tokenTTL)
	a.mu.Unlock()
	return token, true
}

func (a *Auth) ValidateToken(token string) bool {
	a.mu.RLock()
	exp, ok := a.tokens[token]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		a.mu.Lock()
		delete(a.tokens, token)
		a.mu.Unlock()
		return false
	}
	return true
}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil || !a.ValidateToken(cookie.Value) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) ValidateRequest(r *http.Request) bool {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return a.ValidateToken(cookie.Value)
}

func SetTokenCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(tokenTTL.Seconds()),
	})
}
