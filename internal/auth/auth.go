package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/db"
	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "agentserver-token"
	tokenTTL   = 7 * 24 * time.Hour
)

type contextKey string

const userIDKey contextKey = "userID"

type Auth struct {
	db *db.DB
}

func New(database *db.DB) *Auth {
	return &Auth{db: database}
}

// Register creates a new user with a bcrypt-hashed password.
func (a *Auth) Register(id, username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return a.db.CreateUser(id, username, string(hash))
}

// Login verifies credentials and returns a token.
func (a *Auth) Login(username, password string) (string, string, bool) {
	user, err := a.db.GetUserByUsername(username)
	if err != nil || user == nil {
		return "", "", false
	}
	if user.PasswordHash == nil {
		return "", "", false
	}
	if bcrypt.CompareHashAndPassword([]byte(*user.PasswordHash), []byte(password)) != nil {
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

// Middleware enforces authentication and injects user ID into context.
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

// GetUserByID returns user info by ID.
func (a *Auth) GetUserByID(id string) (*db.User, error) {
	return a.db.GetUserByID(id)
}

// GetUserByUsername returns user info by username.
func (a *Auth) GetUserByUsername(username string) (*db.User, error) {
	return a.db.GetUserByUsername(username)
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
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(tokenTTL.Seconds()),
	})
}
