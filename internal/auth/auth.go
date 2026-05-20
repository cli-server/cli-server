package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/db"
	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "agentserver-token"
	tokenTTL   = 7 * 24 * time.Hour
)

// cookieDomain returns the Domain attribute for the session cookie.
// Empty (the default) means a host-only cookie scoped to the exact
// host that set it. When set to e.g. ".agent.cs.ac.cn" it lets the
// cookie cross subdomains — necessary for the codex-auth subdomain to
// SSO with the main app session.
func cookieDomain() string {
	return os.Getenv("AGENTSERVER_COOKIE_DOMAIN")
}

type contextKey string

const userIDKey contextKey = "userID"

type Auth struct {
	db *db.DB
}

func New(database *db.DB) *Auth {
	return &Auth{db: database}
}

// Register creates a new user with a bcrypt-hashed password.
func (a *Auth) Register(id, email, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := a.db.CreateUser(id, email, string(hash)); err != nil {
		return err
	}
	return nil
}

// Login verifies credentials by email and returns a token.
func (a *Auth) Login(email, password string) (string, string, bool) {
	user, err := a.db.GetUserByEmail(email)
	if err != nil || user == nil {
		return "", "", false
	}
	hash, err := a.db.GetPasswordHash(user.ID)
	if err != nil || hash == nil {
		return "", "", false
	}
	if bcrypt.CompareHashAndPassword([]byte(*hash), []byte(password)) != nil {
		return "", "", false
	}
	token, err := a.IssueToken(user.ID)
	if err != nil {
		return "", "", false
	}
	return token, user.ID, true
}

// IssueToken generates a random token, stores it, and returns it.
func (a *Auth) IssueToken(userID string) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	if err := a.db.CreateToken(token, userID, time.Now().Add(tokenTTL)); err != nil {
		return "", err
	}
	return token, nil
}

// ValidateToken checks the token against the database and returns the user ID.
func (a *Auth) ValidateToken(token string) (string, bool) {
	userID, err := a.db.ValidateToken(token)
	if err != nil || userID == "" {
		return "", false
	}
	return userID, true
}

// Middleware authenticates web requests via session cookie. The TUI / agent
// CLI does NOT use this — it goes through BearerMiddleware on /api/agents/*.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		userID, ok := a.ValidateToken(cookie.Value)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// BearerMiddleware authenticates TUI / agent CLI requests via OAuth Bearer
// token, using Hydra introspection. The web app does NOT use this — it goes
// through Middleware (cookie auth). Token must be Active and have a non-empty
// Subject (= user ID), which is then injected into request context under the
// same key Middleware uses.
func BearerMiddleware(h *HydraClient) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(authz, "Bearer ")
			intro, err := h.IntrospectToken(token)
			if err != nil || !intro.Active || intro.Subject == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), userIDKey, intro.Subject)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ValidateRequest checks whether a request has a valid auth cookie and returns the user ID.
func (a *Auth) ValidateRequest(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	return a.ValidateToken(cookie.Value)
}

// UserIDFromContext extracts the user ID set by Middleware.
func UserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userIDKey).(string)
	return v
}

// ContextWithUserID returns a copy of ctx with userID injected under the same
// key that Middleware uses. Intended for use in tests that bypass the real
// auth middleware.
func ContextWithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// GetUserByID returns user info by ID.
func (a *Auth) GetUserByID(id string) (*db.User, error) {
	return a.db.GetUserByID(id)
}

// GetUserByEmail returns user info by email.
func (a *Auth) GetUserByEmail(email string) (*db.User, error) {
	return a.db.GetUserByEmail(email)
}

// DB returns the underlying database for use by other auth subsystems.
func (a *Auth) DB() *db.DB {
	return a.db
}

func SetTokenCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		Domain:   cookieDomain(),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(tokenTTL.Seconds()),
	})
}
