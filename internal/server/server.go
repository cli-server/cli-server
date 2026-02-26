package server

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/imryao/cli-server/internal/auth"
	"github.com/imryao/cli-server/internal/db"
	"github.com/imryao/cli-server/internal/process"
	"github.com/imryao/cli-server/internal/session"
	"github.com/imryao/cli-server/internal/storage"
	"github.com/imryao/cli-server/internal/ws"
)

type Server struct {
	Auth           *auth.Auth
	OIDC           *auth.OIDCManager
	DB             *db.DB
	Sessions       *session.Store
	ProcessManager process.Manager
	DriveManager   storage.DriveManager
	WSHandler      *ws.Handler
	StaticFS       fs.FS
	SidecarProxy   *httputil.ReverseProxy
	// activityThrottle prevents excessive DB writes for activity tracking.
	activityMu     sync.Mutex
	activityLast   map[string]time.Time
}

func New(a *auth.Auth, oidcMgr *auth.OIDCManager, database *db.DB, sessionStore *session.Store, processManager process.Manager, driveManager storage.DriveManager, staticFS fs.FS) *Server {
	sidecarURL := os.Getenv("SIDECAR_URL")
	if sidecarURL == "" {
		sidecarURL = "http://localhost:8081"
	}
	target, _ := url.Parse(sidecarURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Allow SSE streaming: disable response buffering.
	proxy.FlushInterval = -1

	s := &Server{
		Auth:           a,
		OIDC:           oidcMgr,
		DB:             database,
		Sessions:       sessionStore,
		ProcessManager: processManager,
		DriveManager:   driveManager,
		StaticFS:       staticFS,
		SidecarProxy:   proxy,
		activityLast:   make(map[string]time.Time),
	}
	s.WSHandler = &ws.Handler{
		Sessions:       sessionStore,
		ProcessManager: processManager,
		OnActivity:     s.throttledActivity,
	}
	return s
}

// throttledActivity updates activity at most once per 30 seconds per session.
func (s *Server) throttledActivity(sessionID string) {
	s.activityMu.Lock()
	last, ok := s.activityLast[sessionID]
	now := time.Now()
	if ok && now.Sub(last) < 30*time.Second {
		s.activityMu.Unlock()
		return
	}
	s.activityLast[sessionID] = now
	s.activityMu.Unlock()
	s.Sessions.UpdateActivity(sessionID)
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Health endpoint (no auth required, for K8s probes)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Auth endpoints (no auth required)
	r.Post("/api/auth/login", s.handleLogin)
	r.Post("/api/auth/register", s.handleRegister)
	r.Get("/api/auth/check", s.handleAuthCheck)
	r.Post("/api/auth/logout", s.handleLogout)

	// OIDC endpoints (no auth required)
	if s.OIDC != nil {
		r.Get("/api/auth/oidc/providers", s.OIDC.HandleProviders)
		r.Get("/api/auth/oidc/{provider}/login", s.handleOIDCLogin)
		r.Get("/api/auth/oidc/{provider}/callback", s.handleOIDCCallback)
	} else {
		r.Get("/api/auth/oidc/providers", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"providers": []string{}})
		})
	}

	// Protected API routes
	r.Group(func(r chi.Router) {
		r.Use(s.Auth.Middleware)

		r.Get("/api/auth/me", s.handleMe)
		r.Get("/api/sessions", s.handleListSessions)
		r.Post("/api/sessions", s.handleCreateSession)
		r.Get("/api/sessions/{id}", s.handleGetSession)
		r.Delete("/api/sessions/{id}", s.handleDeleteSession)
		r.Post("/api/sessions/{id}/pause", s.handlePauseSession)
		r.Post("/api/sessions/{id}/resume", s.handleResumeSession)

		// Messages endpoint (reads from shared DB)
		r.Get("/api/sessions/{id}/messages", s.handleListMessages)

		// Chat proxy to Python sidecar
		r.Post("/api/sessions/{id}/chat", s.handleChatProxy)
		r.Get("/api/sessions/{id}/stream", s.handleStreamProxy)
		r.Delete("/api/sessions/{id}/stream", s.handleStreamDeleteProxy)
	})

	// WebSocket (auth checked inside)
	r.Get("/ws/terminal/{id}", s.handleWebSocket)

	// Static files
	if s.StaticFS != nil {
		fileServer := http.FileServer(http.FS(s.StaticFS))
		r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if path == "/" {
				path = "/index.html"
			}
			if _, err := fs.Stat(s.StaticFS, path[1:]); err != nil {
				r.URL.Path = "/"
			}
			fileServer.ServeHTTP(w, r)
		})
	}

	return r
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token, _, ok := s.Auth.Login(req.Username, req.Password)
	if !ok {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	auth.SetTokenCookie(w, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Username == "" || req.Password == "" {
		http.Error(w, "username and password required", http.StatusBadRequest)
		return
	}

	// Check if user already exists.
	existing, err := s.Auth.GetUserByUsername(req.Username)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		http.Error(w, "username already taken", http.StatusConflict)
		return
	}

	id := uuid.New().String()
	if err := s.Auth.Register(id, req.Username, req.Password); err != nil {
		log.Printf("register error: %v", err)
		http.Error(w, "failed to create user", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "username": req.Username})
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Auth.ValidateRequest(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "cli-server-token",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	user, err := s.Auth.GetUserByID(userID)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       user.ID,
		"username": user.Username,
		"email":    user.Email,
	})
}

type sessionResponse struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
	SandboxName    string  `json:"sandboxName,omitempty"`
	CreatedAt      string  `json:"createdAt"`
	LastActivityAt *string `json:"lastActivityAt"`
	PausedAt       *string `json:"pausedAt"`
}

func toSessionResponse(sess *session.Session) sessionResponse {
	resp := sessionResponse{
		ID:          sess.ID,
		Name:        sess.Name,
		Status:      sess.Status,
		SandboxName: sess.SandboxName,
		CreatedAt:   sess.CreatedAt.Format(time.RFC3339),
	}
	if sess.LastActivityAt != nil {
		s := sess.LastActivityAt.Format(time.RFC3339)
		resp.LastActivityAt = &s
	}
	if sess.PausedAt != nil {
		s := sess.PausedAt.Format(time.RFC3339)
		resp.PausedAt = &s
	}
	return resp
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	sessions := s.Sessions.List(userID)
	resp := make([]sessionResponse, len(sessions))
	for i, sess := range sessions {
		resp[i] = toSessionResponse(sess)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	sess, ok := s.Sessions.Get(id)
	if !ok || sess.UserID != userID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toSessionResponse(sess))
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Name = "New Session"
	}
	if req.Name == "" {
		req.Name = "New Session"
	}

	// Ensure user drive exists.
	userDrivePVC, err := s.DriveManager.EnsureDrive(r.Context(), userID)
	if err != nil {
		log.Printf("failed to ensure user drive for %s: %v", userID, err)
		// Non-fatal: session can still work without user drive.
	}

	id := uuid.New().String()
	sandboxName := "cli-session-" + shortID(id)

	sess, err := s.Sessions.Create(id, userID, req.Name, sandboxName)
	if err != nil {
		log.Printf("failed to create session: %v", err)
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	// Start container asynchronously.
	go func() {
		var podIP string
		// Use StartContainerWithIP if available (K8s backend) to get the pod IP.
		if sc, ok := s.ProcessManager.(interface {
			StartContainerWithIP(string, process.StartOptions) (string, error)
		}); ok {
			var err error
			podIP, err = sc.StartContainerWithIP(id, process.StartOptions{UserDrivePVC: userDrivePVC})
			if err != nil {
				log.Printf("failed to start container for session %s: %v", id, err)
				s.Sessions.Delete(id)
				return
			}
		} else {
			if err := s.ProcessManager.StartContainer(id, process.StartOptions{UserDrivePVC: userDrivePVC}); err != nil {
				log.Printf("failed to start container for session %s: %v", id, err)
				s.Sessions.Delete(id)
				return
			}
		}
		if podIP != "" {
			if err := s.DB.UpdateSessionPodIP(id, podIP); err != nil {
				log.Printf("failed to update pod IP for session %s: %v", id, err)
			}
		}
		s.Sessions.UpdateStatus(id, session.StatusRunning)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(toSessionResponse(sess))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	sess, ok := s.Sessions.Get(id)
	if !ok || sess.UserID != userID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Handle based on session status.
	switch sess.Status {
	case session.StatusRunning:
		s.ProcessManager.Stop(id)
	case session.StatusPaused:
		// Delete the sandbox/container directly by name.
		if sess.SandboxName != "" {
			switch mgr := s.ProcessManager.(type) {
			case interface{ StopBySandboxName(string) error }:
				mgr.StopBySandboxName(sess.SandboxName)
			case interface{ StopByContainerName(string) error }:
				mgr.StopByContainerName(sess.SandboxName)
			}
		}
	}

	if err := s.Sessions.Delete(id); err != nil {
		log.Printf("failed to delete session %s: %v", id, err)
		http.Error(w, "failed to delete session", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePauseSession(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	sess, ok := s.Sessions.Get(id)
	if !ok || sess.UserID != userID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if !session.ValidTransition(sess.Status, session.StatusPausing) {
		http.Error(w, "session cannot be paused in current state: "+sess.Status, http.StatusConflict)
		return
	}

	// Transition to pausing.
	if err := s.Sessions.UpdateStatus(id, session.StatusPausing); err != nil {
		http.Error(w, "failed to update status", http.StatusInternalServerError)
		return
	}

	// Pause asynchronously.
	go func() {
		if err := s.ProcessManager.Pause(id); err != nil {
			log.Printf("failed to pause session %s: %v", id, err)
			s.Sessions.UpdateStatus(id, session.StatusRunning)
			return
		}
		s.Sessions.UpdateStatus(id, session.StatusPaused)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "pausing"})
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	sess, ok := s.Sessions.Get(id)
	if !ok || sess.UserID != userID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if !session.ValidTransition(sess.Status, session.StatusResuming) {
		http.Error(w, "session cannot be resumed in current state: "+sess.Status, http.StatusConflict)
		return
	}

	if !session.ValidTransition(sess.Status, session.StatusResuming) {
		http.Error(w, "session cannot be resumed in current state: "+sess.Status, http.StatusConflict)
		return
	}

	// Transition to resuming.
	if err := s.Sessions.UpdateStatus(id, session.StatusResuming); err != nil {
		http.Error(w, "failed to update status", http.StatusInternalServerError)
		return
	}

	// Resume asynchronously.
	go func() {
		var err error
		var podIP string
		// Use ResumeContainerWithIP if available (K8s backend).
		if rc, ok := s.ProcessManager.(interface {
			ResumeContainerWithIP(string) (string, error)
		}); ok {
			podIP, err = rc.ResumeContainerWithIP(id)
		} else if rc, ok := s.ProcessManager.(interface{ ResumeContainer(string) error }); ok {
			err = rc.ResumeContainer(id)
		} else {
			err = s.ProcessManager.StartContainer(id, process.StartOptions{})
		}
		if err != nil {
			log.Printf("failed to resume session %s: %v", id, err)
			s.Sessions.UpdateStatus(id, session.StatusPaused)
			return
		}
		if podIP != "" {
			if err := s.DB.UpdateSessionPodIP(id, podIP); err != nil {
				log.Printf("failed to update pod IP for session %s: %v", id, err)
			}
		}
		s.Sessions.UpdateStatus(id, session.StatusRunning)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resuming"})
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.Auth.ValidateRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")

	// Verify ownership.
	sess, found := s.Sessions.Get(id)
	if !found || sess.UserID != userID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	s.WSHandler.ServeHTTP(w, r, id)
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	sess, ok := s.Sessions.Get(id)
	if !ok || sess.UserID != userID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	messages, err := s.DB.ListMessages(id)
	if err != nil {
		log.Printf("failed to list messages for session %s: %v", id, err)
		http.Error(w, "failed to list messages", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
}

// proxySidecar forwards requests to the TypeScript sidecar, injecting session context.
func (s *Server) proxySidecar(w http.ResponseWriter, r *http.Request, targetPath string) {
	userID := auth.UserIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	sess, ok := s.Sessions.Get(id)
	if !ok || sess.UserID != userID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	r.URL.Path = targetPath
	r.Header.Set("X-Session-ID", id)
	r.Header.Set("X-User-ID", userID)
	if sess.SandboxName != "" {
		r.Header.Set("X-Sandbox-Name", sess.SandboxName)
	}
	if sess.PodIP != "" {
		r.Header.Set("X-Pod-IP", sess.PodIP)
	}

	s.SidecarProxy.ServeHTTP(w, r)
}

func (s *Server) handleChatProxy(w http.ResponseWriter, r *http.Request) {
	s.proxySidecar(w, r, "/chat")
}

func (s *Server) handleStreamProxy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.proxySidecar(w, r, "/stream/"+id)
}

func (s *Server) handleStreamDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.proxySidecar(w, r, "/stream/"+id)
}

func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	s.OIDC.HandleLogin(w, r, provider)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	s.OIDC.HandleCallback(w, r, provider)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
