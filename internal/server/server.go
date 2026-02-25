package server

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/imryao/cli-server/internal/auth"
	"github.com/imryao/cli-server/internal/process"
	"github.com/imryao/cli-server/internal/session"
	"github.com/imryao/cli-server/internal/ws"
)

type Server struct {
	Auth           *auth.Auth
	Sessions       *session.Store
	ProcessManager process.Manager
	WSHandler      *ws.Handler
	StaticFS       fs.FS
}

func New(password string, processManager process.Manager, staticFS fs.FS) *Server {
	a := auth.New(password)
	sessions := session.NewStore()
	wsHandler := &ws.Handler{Sessions: sessions, ProcessManager: processManager}

	return &Server{
		Auth:           a,
		Sessions:       sessions,
		ProcessManager: processManager,
		WSHandler:      wsHandler,
		StaticFS:       staticFS,
	}
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
	r.Get("/api/auth/check", s.handleAuthCheck)

	// Protected API routes
	r.Group(func(r chi.Router) {
		r.Use(s.Auth.Middleware)

		r.Get("/api/sessions", s.handleListSessions)
		r.Post("/api/sessions", s.handleCreateSession)
		r.Delete("/api/sessions/{id}", s.handleDeleteSession)
	})

	// WebSocket (auth checked inside)
	r.Get("/ws/terminal/{id}", s.handleWebSocket)

	// Static files
	if s.StaticFS != nil {
		fileServer := http.FileServer(http.FS(s.StaticFS))
		r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			// Try to serve the file; if not found, serve index.html for SPA routing
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
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token, ok := s.Auth.Login(req.Password)
	if !ok {
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}
	auth.SetTokenCookie(w, token)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.ValidateRequest(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.Sessions.List()
	type sessionResp struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		CreatedAt string `json:"createdAt"`
	}
	resp := make([]sessionResp, len(sessions))
	for i, sess := range sessions {
		resp[i] = sessionResp{
			ID:        sess.ID,
			Name:      sess.Name,
			CreatedAt: sess.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Name = "New Session"
	}
	if req.Name == "" {
		req.Name = "New Session"
	}

	id := uuid.New().String()
	sess := s.Sessions.Create(id, req.Name)

	// Start process with claude CLI
	_, err := s.ProcessManager.Start(id, "claude", []string{}, []string{
		"TERM=xterm-256color",
	})
	if err != nil {
		s.Sessions.Delete(id)
		log.Printf("failed to start process: %v", err)
		http.Error(w, "failed to start session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":        sess.ID,
		"name":      sess.Name,
		"createdAt": sess.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.ProcessManager.Stop(id)
	if !s.Sessions.Delete(id) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.ValidateRequest(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	s.WSHandler.ServeHTTP(w, r, id)
}
