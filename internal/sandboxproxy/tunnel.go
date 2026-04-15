package sandboxproxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"encoding/base64"

	"github.com/go-chi/chi/v5"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/tunnel"
	"nhooyr.io/websocket"
)

// handleTunnel upgrades the connection to a WebSocket and serves as the
// server-side endpoint for local agent tunnels.
// The connection is wrapped as net.Conn + yamux for stream multiplexing.
func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxId")
	token := r.URL.Query().Get("token")
	if sandboxID == "" || token == "" {
		http.Error(w, "missing parameters", http.StatusBadRequest)
		return
	}

	// Validate sandbox exists, is local, and tunnel token matches.
	sbx, err := s.DB.GetSandboxByTunnelToken(sandboxID, token)
	if err != nil {
		log.Printf("tunnel auth error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sbx == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Accept WebSocket upgrade.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("tunnel websocket accept error: %v", err)
		return
	}

	// Register tunnel with WSConn + yamux.
	t := s.TunnelRegistry.Register(r.Context(), sandboxID, ws)

	// Set up agent info callback.
	t.OnAgentInfo = func(data json.RawMessage) {
		var info db.AgentInfo
		if err := json.Unmarshal(data, &info); err != nil {
			log.Printf("tunnel %s: failed to unmarshal agent info: %v", sandboxID, err)
			return
		}
		info.SandboxID = sandboxID
		if err := s.DB.UpsertAgentInfo(&info); err != nil {
			log.Printf("tunnel %s: failed to upsert agent info: %v", sandboxID, err)
		}

		// If capabilities present, build and upsert agent card.
		var parsed struct {
			Capabilities *capabilitiesPayload `json:"capabilities"`
		}
		if err := json.Unmarshal(data, &parsed); err == nil && parsed.Capabilities != nil {
			cardJSON := buildCardJSON(parsed.Capabilities, &info)
			if err := s.DB.UpsertAgentCardFromCapabilities(sandboxID, sbx.WorkspaceID, sbx.Name, cardJSON); err != nil {
				log.Printf("tunnel %s: failed to upsert agent card from capabilities: %v", sandboxID, err)
			}
		}
	}

	log.Printf("tunnel connected: sandbox %s", sandboxID)

	// Update sandbox status to running.
	s.Sandboxes.UpdateStatus(sandboxID, sbxstore.StatusRunning)
	s.DB.UpdateSandboxHeartbeat(sandboxID)

	// Heartbeat ticker: update DB heartbeat and send a WebSocket-level
	// ping to keep the connection alive through reverse proxies.
	// yamux keepalive is disabled — the agent sends control streams
	// every 20s for upstream traffic; this ping provides downstream traffic.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.Done():
				return
			case <-ticker.C:
				s.DB.UpdateSandboxHeartbeat(sandboxID)
				// WebSocket-level ping (handled by nhooyr/websocket automatically).
				pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
				if err := ws.Ping(pingCtx); err != nil {
					pingCancel()
					log.Printf("tunnel %s: ping failed: %v", sandboxID, err)
					cancel()
					return
				}
				pingCancel()
			}
		}
	}()

	// Wait for tunnel to close (yamux session ends).
	select {
	case <-ctx.Done():
	case <-t.Done():
	}

	// Cleanup: only set offline if this tunnel is still the active one.
	wasActive := s.TunnelRegistry.Unregister(sandboxID, t)
	t.Close()

	if wasActive {
		s.Sandboxes.UpdateStatus(sandboxID, sbxstore.StatusOffline)
	}
	log.Printf("tunnel disconnected: sandbox %s (was_active=%v)", sandboxID, wasActive)
}

// proxyViaTunnel forwards an HTTP request through the yamux tunnel to the local agent.
func (s *Server) proxyViaTunnel(w http.ResponseWriter, r *http.Request, sbx *sbxstore.Sandbox, t *tunnel.Tunnel) {
	// Read request body.
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
			return
		}
	}

	// Build request headers.
	headers := make(map[string]string)
	for key, vals := range r.Header {
		if len(vals) > 0 {
			headers[key] = vals[0]
		}
	}

	// Inject opencode Basic Auth (only for opencode type sandboxes).
	if sbx.Type == "opencode" && sbx.OpencodeToken != "" {
		cred := base64.StdEncoding.EncodeToString([]byte("opencode:" + sbx.OpencodeToken))
		headers["Authorization"] = "Basic " + cred
	}

	meta := tunnel.HTTPStreamMeta{
		Method:  r.Method,
		Path:    r.URL.RequestURI(),
		Headers: headers,
	}

	// Track activity.
	s.throttledActivity(sbx.ID)

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	respMeta, respBody, err := t.OpenHTTPStream(ctx, meta, body)
	if err != nil {
		log.Printf("tunnel proxy error for %s: %v", t.SandboxID, err)
		http.Error(w, "tunnel proxy error", http.StatusBadGateway)
		return
	}
	defer respBody.Close()

	// Write response headers.
	for k, v := range respMeta.Headers {
		w.Header().Set(k, v)
	}
	if respMeta.Status > 0 {
		w.WriteHeader(respMeta.Status)
	}

	// Stream response body with flushing for SSE support.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, readErr := respBody.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
}
